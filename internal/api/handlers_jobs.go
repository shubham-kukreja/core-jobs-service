package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/auth"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// JobRuns is the run store the API reads; jobs.RunStore satisfies it.
type JobRuns interface {
	jobs.Runs
	UUIDs(ctx context.Context, ids []int64) (map[int64]uuid.UUID, error)
}

// JobDeps are the job-system collaborators. Uploads and Mappings are nil
// when no blob store is configured; their endpoints then return 409
// not_available.
type JobDeps struct {
	Jobs      jobs.Enqueuer
	Runs      JobRuns
	Schedules jobs.Schedules
	Kinds     *jobs.Registry
	Uploads   ingest.Uploads
	Mappings  ingest.Mappings
}

const (
	sseMaxDuration  = 30 * time.Minute
	ssePingInterval = 15 * time.Second
	qualityKind     = "quality.check"
)

func pathUUID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperr.New(apperr.NotFound, "Not found")
	}
	return id, nil
}

// ---------------------------------------------------------------- output

type jobRunOut struct {
	ID              uuid.UUID         `json:"id"`
	Kind            string            `json:"kind"`
	Args            json.RawMessage   `json:"args,omitempty"`
	Status          jobs.Status       `json:"status"`
	Trigger         jobs.TriggerType  `json:"trigger"`
	Queue           string            `json:"queue"`
	User            *string           `json:"user"`
	Attempt         int               `json:"attempt"`
	Progress        int               `json:"progress"`
	Message         *string           `json:"message"`
	Result          json.RawMessage   `json:"result"`
	Error           *jobs.RunError    `json:"error"`
	CancelRequested bool              `json:"cancel_requested"`
	ParentID        *uuid.UUID        `json:"parent_id"`
	RootID          *uuid.UUID        `json:"root_id"`
	QueuedAt        time.Time         `json:"queued_at"`
	StartedAt       *time.Time        `json:"started_at"`
	FinishedAt      *time.Time        `json:"finished_at"`
	Links           map[string]string `json:"links"`
}

// runRenderer resolves run ids and user emails once per request.
type runRenderer struct {
	s     *Server
	uuids map[int64]uuid.UUID
	email map[int64]*string
}

func (s *Server) newRenderer(ctx context.Context, runs []jobs.JobRun) (*runRenderer, error) {
	var ids []int64
	for _, r := range runs {
		if r.ParentID != nil {
			ids = append(ids, *r.ParentID)
		}
		if r.RootID != nil {
			ids = append(ids, *r.RootID)
		}
	}
	m, err := s.jobs.Runs.UUIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	return &runRenderer{s: s, uuids: m, email: map[int64]*string{}}, nil
}

func (rr *runRenderer) user(ctx context.Context, id *int64) *string {
	if id == nil {
		return nil
	}
	if e, ok := rr.email[*id]; ok {
		return e
	}
	var e *string
	if u, err := rr.s.users.Get(ctx, *id); err == nil {
		e = &u.Email
	}
	rr.email[*id] = e
	return e
}

func (rr *runRenderer) ref(id *int64) *uuid.UUID {
	if id == nil {
		return nil
	}
	if u, ok := rr.uuids[*id]; ok {
		return &u
	}
	return nil
}

func (rr *runRenderer) run(ctx context.Context, r jobs.JobRun) jobRunOut {
	out := jobRunOut{
		ID: r.UUID, Kind: r.Kind, Args: r.Args, Status: r.Status, Trigger: r.Trigger, Queue: r.Queue,
		User: rr.user(ctx, r.UserID), Attempt: r.Attempt, Progress: r.Progress, Result: r.Result, Error: r.Error,
		CancelRequested: r.CancelRequested, ParentID: rr.ref(r.ParentID), RootID: rr.ref(r.RootID),
		QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Links: map[string]string{"self": "/v1/jobs/" + r.UUID.String(), "events": "/v1/jobs/" + r.UUID.String() + "/events"},
	}
	if r.Message != "" {
		out.Message = &r.Message
	}
	if len(out.Result) == 0 {
		out.Result = json.RawMessage("null")
	}
	if out.RootID == nil && r.RootID != nil && *r.RootID == r.ID {
		out.RootID = &out.ID
	}
	return out
}

func (s *Server) renderOne(ctx context.Context, r jobs.JobRun) (jobRunOut, error) {
	rr, err := s.newRenderer(ctx, []jobs.JobRun{r})
	if err != nil {
		return jobRunOut{}, err
	}
	return rr.run(ctx, r), nil
}

// acceptedRun is the 202 body for a run enqueued in an uncommitted
// transaction: its row is not readable yet, so it is built from the ref.
func acceptedRun(ref jobs.RunRef, spec jobs.Spec, args json.RawMessage, trig jobs.TriggerType, email string, now time.Time) map[string]any {
	e := email
	return map[string]any{"job_run": jobRunOut{
		ID: ref.UUID, Kind: spec.Kind, Args: args, Status: ref.Status, Trigger: trig, Queue: spec.Queue, User: &e,
		Result: json.RawMessage("null"), QueuedAt: now, RootID: &ref.UUID,
		Links: map[string]string{"self": "/v1/jobs/" + ref.UUID.String(), "events": "/v1/jobs/" + ref.UUID.String() + "/events"},
	}}
}

// ---------------------------------------------------------------- job kinds

func (s *Server) listJobKinds(w http.ResponseWriter, _ *http.Request) {
	kinds := s.jobs.Kinds.All()
	out := make([]map[string]any, 0, len(kinds))
	for _, k := range kinds {
		roles := k.Spec.Roles
		if roles == nil {
			roles = []string{}
		}
		out = append(out, map[string]any{
			"kind": k.Spec.Kind, "queue": k.Spec.Queue, "user_triggerable": k.Spec.UserTriggerable,
			"roles": roles, "args_version": k.Spec.ArgsVersion, "args_schema": k.Spec.ArgsSchema,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ---------------------------------------------------------------- jobs

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	const scope = "jobs"
	page, err := queryPage(r, s, scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := jobs.RunFilter{Kind: q.Get("kind"), Status: jobs.Status(q.Get("status")), Trigger: jobs.TriggerType(q.Get("trigger")), Page: page}
	var probs problems
	if f.Kind != "" {
		probs.key("kind", f.Kind)
	}
	if f.Status != "" {
		probs.oneOf("status", string(f.Status), "queued", "running", "succeeded", "failed", "cancelled", "skipped")
	}
	if f.Trigger != "" {
		probs.oneOf("trigger", string(f.Trigger), "schedule", "event", "user", "chain", "system")
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			probs.add("since", "invalid_datetime")
		}
		f.Since = &t
	}
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	if v := q.Get("root_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			s.writeError(w, r, apperr.New(apperr.InvalidParams, "Invalid root_id", apperr.Detail{Field: "root_id", Reason: "invalid_uuid"}))
			return
		}
		root, err := s.jobs.Runs.Get(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"data": []jobRunOut{}, "next_cursor": nil})
			return
		}
		f.RootID = &root.ID
	}
	res, err := s.jobs.Runs.List(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rr, err := s.newRenderer(r.Context(), res.Items)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]jobRunOut, 0, len(res.Items))
	for _, run := range res.Items {
		out = append(out, rr.run(r.Context(), run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": s.cursors.Encode(scope, res.Next)})
}

type jobSubmit struct {
	Kind string          `json:"kind"`
	Args json.RawMessage `json:"args"`
}

// allowedRole reports whether role meets any of the kind's roles.
func allowedRole(role auth.Role, roles []string) bool {
	for _, r := range roles {
		if role.AtLeast(auth.Role(r)) {
			return true
		}
	}
	return false
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in jobSubmit
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var probs problems
	probs.key("kind", in.Kind)
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	kind, ok := s.jobs.Kinds.Get(in.Kind)
	if !ok {
		s.writeError(w, r, apperr.New(apperr.ValidationFailed, "Unknown job kind", apperr.Detail{Field: "kind", Reason: "unknown"}))
		return
	}
	spec := kind.Spec
	if !spec.UserTriggerable || !spec.Allows(jobs.TriggerUser) {
		s.writeError(w, r, apperr.New(apperr.NotUserTriggerable, fmt.Sprintf("%s cannot be started by a user", spec.Kind)))
		return
	}
	p := mustPrincipal(r.Context())
	if !allowedRole(p.Role, spec.Roles) {
		s.writeError(w, r, apperr.New(apperr.Forbidden, "Your role cannot start this job"))
		return
	}
	if len(in.Args) == 0 || string(in.Args) == "null" {
		in.Args = json.RawMessage("{}")
	}
	if err := kind.ValidateArgs(in.Args); err != nil {
		s.writeError(w, r, apperr.Wrap(err, apperr.ValidationFailed, "Invalid job args"))
		return
	}
	args, err := kind.Decode(in.Args)
	if err != nil {
		s.writeError(w, r, apperr.Wrap(err, apperr.ValidationFailed, "Invalid job args"))
		return
	}
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		ref, err := s.jobs.Jobs.EnqueueTx(ctx, tx, args, jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(p.UserID))
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "job", ref.UUID.String(), "submit", nil, map[string]any{"kind": spec.Kind})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusAccepted, Body: acceptedRun(ref, spec, in.Args, jobs.TriggerUser, p.Email, s.clock.Now())}, nil
	})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.jobs.Runs.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.renderOne(r.Context(), run)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type jobTreeOut struct {
	ID       uuid.UUID        `json:"id"`
	Kind     string           `json:"kind"`
	Trigger  jobs.TriggerType `json:"trigger"`
	Status   jobs.Status      `json:"status"`
	Result   json.RawMessage  `json:"result"`
	Children []jobTreeOut     `json:"children"`
}

func treeOut(t jobs.RunTree) jobTreeOut {
	out := jobTreeOut{ID: t.UUID, Kind: t.Kind, Trigger: t.Trigger, Status: t.Status, Result: t.Result, Children: []jobTreeOut{}}
	if len(out.Result) == 0 {
		out.Result = json.RawMessage("null")
	}
	for _, c := range t.Children {
		out.Children = append(out.Children, treeOut(c))
	}
	return out
}

func (s *Server) getJobTree(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	depth, err := queryInt(r, "depth", 5, 1, jobs.MaxTreeDepth)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.jobs.Runs.Tree(r.Context(), id, depth)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, treeOut(t))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	run, err := s.jobs.Runs.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !p.Role.AtLeast(auth.RoleAdmin) && (run.UserID == nil || *run.UserID != p.UserID) {
		s.writeError(w, r, apperr.New(apperr.Forbidden, "Only the job's owner or an admin can cancel it"))
		return
	}
	out, err := s.renderOne(r.Context(), run)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		if err := s.jobs.Runs.RequestCancel(ctx, tx, id); err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "job", id.String(), "cancel", nil, nil)); err != nil {
			return txResult{}, err
		}
		out.CancelRequested = true
		return txResult{Status: http.StatusAccepted, Body: map[string]any{"job_run": out}}, nil
	})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	run, err := s.jobs.Runs.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	kind, ok := s.jobs.Kinds.Get(run.Kind)
	if !ok {
		s.writeError(w, r, apperr.New(apperr.Conflict, "This job kind is no longer registered"))
		return
	}
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		ref, err := s.jobs.Runs.Retry(ctx, tx, id, p.UserID)
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "job", id.String(), "retry", nil, map[string]any{"new_run": ref.UUID})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusAccepted, Body: acceptedRun(ref, kind.Spec, run.Args, jobs.TriggerUser, p.Email, s.clock.Now())}, nil
	})
}

// streamJobEvents serves Server-Sent Events until the run ends, the client
// leaves, or sseMaxDuration passes.
func (s *Server) streamJobEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sseMaxDuration)
	defer cancel()
	updates, err := s.jobs.Runs.Watch(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // the server's write timeout would cut the stream
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case u, ok := <-updates:
			if !ok {
				return
			}
			var data any
			if u.Kind == "progress" {
				data = map[string]any{"progress": u.Progress, "message": u.Message}
			} else {
				res := u.Result
				if len(res) == 0 {
					res = json.RawMessage("null")
				}
				data = map[string]any{"status": u.Status, "progress": u.Progress, "result": res}
			}
			b, err := json.Marshal(data)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", u.Kind, b); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- schedules

type scheduleOut struct {
	ID            uuid.UUID       `json:"id"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Args          json.RawMessage `json:"args"`
	ScheduleType  string          `json:"schedule_type"`
	Expr          string          `json:"expr"`
	Timezone      string          `json:"timezone"`
	OverlapPolicy string          `json:"overlap_policy"`
	CatchupPolicy string          `json:"catchup_policy"`
	JitterSecs    int             `json:"jitter_secs"`
	Enabled       bool            `json:"enabled"`
	NextRunAt     time.Time       `json:"next_run_at"`
	LastRun       any             `json:"last_run"`
}

func (s *Server) schedulesOut(ctx context.Context, list []jobs.Schedule) ([]scheduleOut, error) {
	var ids []int64
	for _, sc := range list {
		if sc.LastRunID != nil {
			ids = append(ids, *sc.LastRunID)
		}
	}
	m, err := s.jobs.Runs.UUIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]scheduleOut, 0, len(list))
	for _, sc := range list {
		o := scheduleOut{ID: sc.UUID, Name: sc.Name, Kind: sc.Kind, Args: sc.Args, ScheduleType: sc.ScheduleType, Expr: sc.Expr,
			Timezone: sc.Timezone, OverlapPolicy: sc.OverlapPolicy, CatchupPolicy: sc.CatchupPolicy, JitterSecs: sc.JitterSecs,
			Enabled: sc.Enabled, NextRunAt: sc.NextRunAt}
		if len(o.Args) == 0 {
			o.Args = json.RawMessage("{}")
		}
		if sc.LastRunID != nil {
			if u, ok := m[*sc.LastRunID]; ok {
				if run, err := s.jobs.Runs.Get(ctx, u); err == nil {
					o.LastRun = map[string]any{"id": u, "kind": run.Kind, "status": run.Status}
				}
			}
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.jobs.Schedules.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.schedulesOut(r.Context(), list)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type scheduleFields struct {
	Name          *string         `json:"name"`
	Kind          *string         `json:"kind"`
	Args          json.RawMessage `json:"args"`
	ScheduleType  *string         `json:"schedule_type"`
	Expr          *string         `json:"expr"`
	Timezone      *string         `json:"timezone"`
	OverlapPolicy *string         `json:"overlap_policy"`
	CatchupPolicy *string         `json:"catchup_policy"`
	JitterSecs    *int            `json:"jitter_secs"`
	Enabled       *bool           `json:"enabled"`
}

func (f scheduleFields) empty() bool {
	return f.Name == nil && f.Kind == nil && len(f.Args) == 0 && f.ScheduleType == nil && f.Expr == nil &&
		f.Timezone == nil && f.OverlapPolicy == nil && f.CatchupPolicy == nil && f.JitterSecs == nil && f.Enabled == nil
}

// apply copies set fields onto sc, validating each against the spec.
func (f scheduleFields) apply(sc *jobs.Schedule, probs *problems) {
	if f.Name != nil {
		sc.Name = *f.Name
		if !scheduleNameRE.MatchString(sc.Name) {
			probs.add("name", "invalid_format")
		}
	}
	if f.Kind != nil {
		sc.Kind = *f.Kind
		probs.key("kind", sc.Kind)
	}
	if len(f.Args) > 0 && string(f.Args) != "null" {
		var m map[string]any
		if json.Unmarshal(f.Args, &m) != nil || len(m) > 50 {
			probs.add("args", "invalid")
		}
		sc.Args = f.Args
	}
	if f.ScheduleType != nil {
		sc.ScheduleType = *f.ScheduleType
		probs.oneOf("schedule_type", sc.ScheduleType, "cron", "interval", "calendar")
	}
	if f.Expr != nil {
		sc.Expr = *f.Expr
		probs.text("expr", sc.Expr, 1, 64)
	}
	if f.Timezone != nil {
		sc.Timezone = *f.Timezone
		probs.text("timezone", sc.Timezone, 1, 64)
	}
	if f.OverlapPolicy != nil {
		sc.OverlapPolicy = *f.OverlapPolicy
		probs.oneOf("overlap_policy", sc.OverlapPolicy, "skip", "queue", "cancel_previous")
	}
	if f.CatchupPolicy != nil {
		sc.CatchupPolicy = *f.CatchupPolicy
		probs.oneOf("catchup_policy", sc.CatchupPolicy, "none", "latest", "all")
	}
	if f.JitterSecs != nil {
		sc.JitterSecs = *f.JitterSecs
		if sc.JitterSecs < 0 || sc.JitterSecs > 3600 {
			probs.add("jitter_secs", "out_of_range")
		}
	}
	if f.Enabled != nil {
		sc.Enabled = *f.Enabled
	}
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request, existing *jobs.Schedule) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in scheduleFields
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc := jobs.Schedule{Enabled: true}
	var probs problems
	status := http.StatusCreated
	if existing != nil {
		sc = *existing
		status = http.StatusOK
		if in.empty() {
			probs.add("body", "empty")
		}
	} else {
		if in.Name == nil {
			probs.add("name", "required")
		}
		if in.Kind == nil {
			probs.add("kind", "required")
		}
		if in.ScheduleType == nil {
			probs.add("schedule_type", "required")
		}
		if in.Expr == nil {
			probs.add("expr", "required")
		}
	}
	in.apply(&sc, &probs)
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	s.mutate(w, r, body, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		saved, err := s.jobs.Schedules.Save(ctx, tx, sc, p.UserID)
		if err != nil {
			return txResult{}, err
		}
		action := "create"
		var before any
		if existing != nil {
			action, before = "update", map[string]any{"expr": existing.Expr, "enabled": existing.Enabled, "args": existing.Args}
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "schedule", saved.UUID.String(), action, before,
			map[string]any{"name": saved.Name, "kind": saved.Kind, "expr": saved.Expr, "enabled": saved.Enabled, "args": saved.Args})); err != nil {
			return txResult{}, err
		}
		out, err := s.schedulesOut(ctx, []jobs.Schedule{saved})
		if err != nil {
			return txResult{}, err
		}
		return txResult{Status: status, Body: out[0]}, nil
	})
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) { s.saveSchedule(w, r, nil) }

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, err := s.jobs.Schedules.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.saveSchedule(w, r, &sc)
}

func (s *Server) setScheduleEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
			if err := s.jobs.Schedules.SetEnabled(ctx, tx, id, enabled); err != nil {
				return txResult{}, err
			}
			action := map[bool]string{true: "resume", false: "pause"}[enabled]
			if err := s.audit.Record(ctx, tx, auditEntry(r, "schedule", id.String(), action, nil, nil)); err != nil {
				return txResult{}, err
			}
			sc, err := s.jobs.Schedules.Get(ctx, id)
			if err != nil {
				return txResult{}, err
			}
			sc.Enabled = enabled
			out, err := s.schedulesOut(ctx, []jobs.Schedule{sc})
			if err != nil {
				return txResult{}, err
			}
			return txResult{Status: http.StatusOK, Body: out[0]}, nil
		})
	}
}

func (s *Server) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, err := s.jobs.Schedules.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	kind, ok := s.jobs.Kinds.Get(sc.Kind)
	if !ok {
		s.writeError(w, r, apperr.New(apperr.Conflict, "This schedule's job kind is not registered"))
		return
	}
	p := mustPrincipal(r.Context())
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		ref, err := s.jobs.Schedules.RunNow(ctx, tx, id, p.UserID)
		if err != nil {
			return txResult{}, err
		}
		if err := s.audit.Record(ctx, tx, auditEntry(r, "schedule", id.String(), "run_now", nil, map[string]any{"run": ref.UUID})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusAccepted, Body: acceptedRun(ref, kind.Spec, sc.Args, jobs.TriggerSchedule, p.Email, s.clock.Now())}, nil
	})
}

func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	n, err := queryInt(r, "n", 5, 1, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	next, err := s.jobs.Schedules.Preview(r.Context(), id, n)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if next == nil {
		next = []time.Time{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": next})
}

// ---------------------------------------------------------------- uploads & mappings

var errIngestOff = errors.New("api: no blob store configured")

func (s *Server) ingestOn(w http.ResponseWriter, r *http.Request) bool {
	if s.jobs.Uploads == nil || s.jobs.Mappings == nil {
		s.writeError(w, r, apperr.Wrap(errIngestOff, apperr.NotAvailable, "File import is not configured"))
		return false
	}
	return true
}

func uploadOut(u ingest.Upload) map[string]any {
	return map[string]any{"id": u.UUID, "filename": u.Filename, "content_type": u.ContentType, "bytes": u.Bytes,
		"sha256": hex.EncodeToString(u.SHA256), "created_at": u.CreatedAt, "expires_at": u.ExpiresAt}
}

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	if !s.ingestOn(w, r) {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeError(w, r, apperr.New(apperr.UnsupportedMedia, "Content-Type must be multipart/form-data"))
		return
	}
	p := mustPrincipal(r.Context())
	for {
		part, err := mr.NextPart()
		if err != nil {
			s.writeError(w, r, apperr.New(apperr.ValidationFailed, "A file part is required", apperr.Detail{Field: "file", Reason: "required"}))
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		up, err := s.jobs.Uploads.Store(r.Context(), ingest.UploadInput{Filename: part.FileName(), Body: part, UploadedBy: p.UserID})
		_ = part.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) || errors.Is(err, ingest.ErrTooLarge) {
				s.writeError(w, r, apperr.New(apperr.PayloadTooLarge, "File too large"))
				return
			}
			s.writeError(w, r, err)
			return
		}
		s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
			if err := s.audit.Record(ctx, tx, auditEntry(r, "upload", up.UUID.String(), "create", nil,
				map[string]any{"filename": up.Filename, "bytes": up.Bytes})); err != nil {
				return txResult{}, err
			}
			return txResult{Status: http.StatusCreated, Body: uploadOut(up)}, nil
		})
		return
	}
}

type mappingInput struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	ingest.Mapping
}

func mappingOut(m ingest.ImportMapping) map[string]any {
	b, _ := json.Marshal(m.Mapping)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	out["key"], out["name"], out["version"] = m.Key, m.Name, m.Version
	out["created_at"], out["updated_at"] = m.CreatedAt, m.UpdatedAt
	return out
}

func (s *Server) listImportMappings(w http.ResponseWriter, r *http.Request) {
	if !s.ingestOn(w, r) {
		return
	}
	list, err := s.jobs.Mappings.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		out = append(out, mappingOut(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) saveImportMapping(w http.ResponseWriter, r *http.Request) {
	if !s.ingestOn(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in mappingInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var probs problems
	probs.text("name", in.Name, 1, 200)
	if in.Key == "" {
		in.Key = slugKey(in.Name)
	}
	probs.key("key", in.Key)
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	m, err := s.jobs.Mappings.Save(r.Context(), in.Key, in.Name, in.Mapping, p.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, nil, func(ctx context.Context, tx pgx.Tx) (txResult, error) {
		if err := s.audit.Record(ctx, tx, auditEntry(r, "import_mapping", m.Key, "save", nil, map[string]any{"version": m.Version})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusCreated, Body: mappingOut(m)}, nil
	})
}

// ---------------------------------------------------------------- quality

// getQualityReport returns the result of the latest succeeded quality.check
// (for as_of when given). 404 until one has run.
func (s *Server) getQualityReport(w http.ResponseWriter, r *http.Request) {
	var want string
	if v := r.URL.Query().Get("as_of"); v != "" {
		d, err := civil.ParseDate(v)
		if err != nil {
			s.writeError(w, r, apperr.New(apperr.InvalidParams, "Invalid as_of", apperr.Detail{Field: "as_of", Reason: "invalid_date"}))
			return
		}
		want = d.String()
	}
	req := paging.Request{Limit: paging.MaxLimit}
	for range 5 { // newest 2,500 runs at most
		page, err := s.jobs.Runs.List(r.Context(), jobs.RunFilter{Kind: qualityKind, Status: jobs.StatusSucceeded, Page: req})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		for _, run := range page.Items {
			var rep struct {
				AsOf string `json:"as_of"`
			}
			if json.Unmarshal(run.Result, &rep) != nil || (want != "" && rep.AsOf != want) {
				continue
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(run.Result)
			return
		}
		if page.Next == "" {
			break
		}
		req.After = page.Next
	}
	s.writeError(w, r, apperr.New(apperr.NotFound, "No quality report yet"))
}

var scheduleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

// slugKey derives a mapping key from its name: "Data Trackers – rates"
// becomes "data_trackers_rates".
func slugKey(name string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			sep = false
		default:
			sep = true
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}
