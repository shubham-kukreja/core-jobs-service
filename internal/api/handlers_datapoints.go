package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Read limits.
const (
	maxPointsPerWrite = 1000
	maxRangeRows      = 10000
	maxSnapshotSeries = 5000
)

func valueOut(v datapoints.Value) any {
	switch {
	case v.Num != nil:
		return *v.Num
	case v.Bool != nil:
		return *v.Bool
	case v.Cat != nil:
		return *v.Cat
	}
	return nil
}

// ---------------------------------------------------------------- write

type pointInput struct {
	Series  string          `json:"series"`
	ObsDate string          `json:"obs_date"`
	Value   json.RawMessage `json:"value"`
	Note    string          `json:"note"`
}

type writeInput struct {
	Source string       `json:"source"`
	Points []pointInput `json:"points"`
}

type rejectionJSON struct {
	Index   int    `json:"index"`
	Series  string `json:"series"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type writeResultJSON struct {
	BatchID    uuid.UUID       `json:"batch_id"`
	Written    int             `json:"written"`
	Skipped    int             `json:"skipped"`
	Rejected   []rejectionJSON `json:"rejected"`
	CascadeJob *jobRefJSON     `json:"cascade_job"`
}

// parseValue accepts a JSON number, boolean or string; the writer checks it
// against the series' type.
func parseValue(raw json.RawMessage) (datapoints.Value, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return datapoints.Value{}, false
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) == 0 || len(s) > 64 {
			return datapoints.Value{}, false
		}
		return datapoints.Cat(s), true
	case 't', 'f':
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return datapoints.Value{}, false
		}
		return datapoints.Bool(b), true
	case 'n':
		return datapoints.Value{}, false
	default:
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return datapoints.Value{}, false
		}
		return datapoints.Num(f), true
	}
}

func (s *Server) writeDatapoints(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in writeInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	var probs problems
	switch in.Source {
	case catalog.SourceKeyAnalyst:
	case catalog.SourceKeyOverride:
		// Plan rule: overriding any value is an admin decision.
		if !isAdmin(p) {
			s.writeError(w, r, apperr.New(apperr.Forbidden, "Requires role admin for source override"))
			return
		}
	default:
		probs.add("source", "not_allowed")
	}
	if len(in.Points) < 1 || len(in.Points) > maxPointsPerWrite {
		probs.add("points", "invalid_length")
	}
	pts := make([]datapoints.Point, 0, len(in.Points))
	for i, pi := range in.Points {
		if !keyRE.MatchString(pi.Series) {
			probs.addAt(i, "series", "invalid_key")
		}
		d, derr := civil.ParseDate(pi.ObsDate)
		if derr != nil || !d.IsValid() {
			probs.addAt(i, "obs_date", "invalid_date")
		}
		v, ok := parseValue(pi.Value)
		if !ok {
			probs.addAt(i, "value", "wrong_type")
		}
		if len([]rune(pi.Note)) > 2000 {
			probs.addAt(i, "note", "invalid_length")
		}
		pts = append(pts, datapoints.Point{SeriesKey: pi.Series, ObsDate: d, Value: v, Note: pi.Note})
	}
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.mutate(w, r, body, func(tx pgx.Tx) (txResult, error) {
		uid := p.UserID
		res, err := s.writer.Write(r.Context(), tx, datapoints.WriteMeta{
			Source: in.Source, ActorID: &uid, Origin: datapoints.OriginPrimary,
		}, pts)
		if err != nil {
			return txResult{}, err
		}
		out := writeResultJSON{BatchID: res.BatchID, Written: res.Written, Skipped: res.Skipped, Rejected: []rejectionJSON{}}
		for _, rj := range res.Rejected {
			out.Rejected = append(out.Rejected, rejectionJSON{Index: rj.Index, Series: rj.SeriesKey, Reason: rj.Reason, Message: rj.Message})
		}
		if len(res.Rejected) == len(pts) {
			details := make([]apperr.Detail, 0, len(res.Rejected))
			for _, rj := range res.Rejected {
				idx := rj.Index
				details = append(details, apperr.Detail{Index: &idx, Field: "value", Reason: rj.Reason})
			}
			return txResult{}, apperr.New(apperr.ValidationFailed, "All points rejected", details...)
		}
		series := make([]string, 0, len(pts))
		for _, pt := range pts {
			series = append(series, pt.SeriesKey)
		}
		sort.Strings(series)
		after := map[string]any{
			"source": in.Source, "written": res.Written, "skipped": res.Skipped,
			"rejected": len(res.Rejected), "series": dedupe(series),
		}
		if err := s.audit.Record(r.Context(), tx, auditEntry(r, "datapoint_batch", res.BatchID.String(), "write", nil, after)); err != nil {
			return txResult{}, err
		}
		// The cascade is enqueued asynchronously by the outbox dispatcher, so
		// there is no job id to return yet.
		return txResult{Status: http.StatusCreated, Body: out}, nil
	})
}

// ---------------------------------------------------------------- reads

type resolvedJSON struct {
	ID            *int64     `json:"id"`
	ObsDate       civil.Date `json:"obs_date"`
	Value         any        `json:"value"`
	Source        *string    `json:"source"`
	KnownAt       *time.Time `json:"known_at"`
	DerivationVer *int       `json:"derivation_ver"`
	Reason        string     `json:"reason,omitempty"`
}

func toResolvedJSON(rv datapoints.Resolved) resolvedJSON {
	id, src, known := rv.ID, rv.Source, rv.KnownAt.UTC()
	out := resolvedJSON{ID: &id, ObsDate: rv.ObsDate, Value: valueOut(rv.Value), Source: &src, KnownAt: &known}
	if rv.DerivVer > 0 {
		v := rv.DerivVer
		out.DerivationVer = &v
	}
	return out
}

func emptyPoint(d civil.Date, reason string) resolvedJSON {
	return resolvedJSON{ObsDate: d, Reason: reason}
}

// seriesFromPath validates the key and confirms the series exists.
func (s *Server) seriesFromPath(r *http.Request) (string, error) {
	key := chi.URLParam(r, "key")
	if !keyRE.MatchString(key) {
		return "", apperr.New(apperr.NotFound, "Not found")
	}
	if _, err := s.catalog.Series(r.Context(), key); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Server) listSeriesDatapoints(w http.ResponseWriter, r *http.Request) {
	key, err := s.seriesFromPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	from, err := parseDate("from", q.Get("from"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	to, err := parseDate("to", q.Get("to"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if to.Before(from) {
		s.writeError(w, r, apperr.New(apperr.ValidationFailed, "to is before from", apperr.Detail{Field: "to", Reason: "out_of_range"}))
		return
	}
	now := s.clock.Now()
	cutoff, explicit, err := queryCutoff(r, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	atCutoff, err := s.reader.Range(r.Context(), key, from, to, datapoints.ReadOpts{Cutoff: cutoff})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var latest []datapoints.Resolved
	if explicit && cutoff.Before(now) {
		// Dates that are known now but were not at the cutoff are listed
		// with a reason, so the caller can see what the cutoff hides.
		if latest, err = s.reader.Range(r.Context(), key, from, to, datapoints.ReadOpts{Cutoff: now}); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	if len(atCutoff)+len(latest) > 2*maxRangeRows {
		s.writeError(w, r, apperr.New(apperr.InvalidParams, "Range too large; narrow from/to", apperr.Detail{Field: "from", Reason: "too_many_rows"}))
		return
	}
	known := make(map[civil.Date]bool, len(atCutoff))
	out := make([]resolvedJSON, 0, len(atCutoff))
	for _, rv := range atCutoff {
		known[rv.ObsDate] = true
		out = append(out, toResolvedJSON(rv))
	}
	for _, rv := range latest {
		if !known[rv.ObsDate] {
			out = append(out, emptyPoint(rv.ObsDate, "not_known_at_cutoff"))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObsDate.Before(out[j].ObsDate) })
	writeJSON(w, http.StatusOK, map[string]any{"series": key, "cutoff": cutoff, "data": out})
}

func (s *Server) getSeriesDatapoint(w http.ResponseWriter, r *http.Request) {
	key, err := s.seriesFromPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := parseDate("obs_date", chi.URLParam(r, "obs_date"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	now := s.clock.Now()
	cutoff, explicit, err := queryCutoff(r, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rv, err := s.reader.At(r.Context(), key, d, datapoints.ReadOpts{Cutoff: cutoff})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rv != nil {
		writeJSON(w, http.StatusOK, toResolvedJSON(*rv))
		return
	}
	reason := "no_value"
	if explicit && cutoff.Before(now) {
		if later, err := s.reader.At(r.Context(), key, d, datapoints.ReadOpts{Cutoff: now}); err == nil && later != nil {
			reason = "not_known_at_cutoff"
		}
	}
	writeJSON(w, http.StatusOK, emptyPoint(d, reason))
}

type versionJSON struct {
	ID           int64     `json:"id"`
	Value        any       `json:"value"`
	Source       string    `json:"source"`
	KnownAt      time.Time `json:"known_at"`
	Actor        string    `json:"actor,omitempty"`
	Note         string    `json:"note,omitempty"`
	Status       string    `json:"status"`
	SupersedesID *int64    `json:"supersedes_id"`
}

func (s *Server) datapointVersions(w http.ResponseWriter, r *http.Request) {
	key, err := s.seriesFromPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := parseDate("obs_date", chi.URLParam(r, "obs_date"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	vs, err := s.reader.Versions(r.Context(), key, d)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resolved, err := s.reader.At(r.Context(), key, d, datapoints.ReadOpts{Cutoff: s.clock.Now()})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	emails := s.actorEmails(r.Context(), vs)
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].KnownAt.After(vs[j].KnownAt) })
	out := make([]versionJSON, 0, len(vs))
	for _, v := range vs {
		vj := versionJSON{
			ID: v.ID, Value: valueOut(v.Value), Source: v.Source, KnownAt: v.KnownAt.UTC(),
			Note: v.Note, Status: v.Status, SupersedesID: v.SupersedesID,
		}
		if v.ActorID != nil {
			vj.Actor = emails[*v.ActorID]
		}
		out = append(out, vj)
	}
	var rid *int64
	if resolved != nil {
		id := resolved.ID
		rid = &id
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved_id": rid, "versions": out})
}

// actorEmails resolves each distinct actor once. Unknown users are omitted
// rather than failing the read.
func (s *Server) actorEmails(ctx context.Context, vs []datapoints.Datapoint) map[int64]string {
	out := map[int64]string{}
	for _, v := range vs {
		if v.ActorID == nil {
			continue
		}
		if _, done := out[*v.ActorID]; done {
			continue
		}
		u, err := s.users.Get(ctx, *v.ActorID)
		if err != nil {
			out[*v.ActorID] = ""
			continue
		}
		out[*v.ActorID] = u.Email
	}
	return out
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		return 0, apperr.New(apperr.ValidationFailed, "Invalid id", apperr.Detail{Field: "id", Reason: "invalid_id"})
	}
	return id, nil
}

type lineageJSON struct {
	ID         int64           `json:"id"`
	Series     string          `json:"series"`
	ObsDate    civil.Date      `json:"obs_date"`
	Value      any             `json:"value"`
	Derivation *derivationMeta `json:"derivation,omitempty"`
	Inputs     []lineageJSON   `json:"inputs,omitempty"`
}

type derivationMeta struct {
	Fn      string `json:"fn"`
	Version int    `json:"version"`
}

func toLineageJSON(l datapoints.Lineage) lineageJSON {
	out := lineageJSON{ID: l.Datapoint.ID, Series: l.Datapoint.SeriesKey, ObsDate: l.Datapoint.ObsDate, Value: valueOut(l.Datapoint.Value)}
	for _, in := range l.Inputs {
		out.Inputs = append(out.Inputs, toLineageJSON(in))
	}
	return out
}

func (s *Server) datapointLineage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	depth, err := queryInt(r, "depth", 5, 1, 10)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	l, err := s.reader.Lineage(r.Context(), id, depth)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := toLineageJSON(l)
	if ser, err := s.catalog.Series(r.Context(), l.Datapoint.SeriesKey); err == nil && ser.Derivation != nil {
		out.Derivation = &derivationMeta{Fn: ser.Derivation.Fn, Version: ser.Derivation.Version}
	}
	writeJSON(w, http.StatusOK, out)
}

type retractInput struct {
	Reason string `json:"reason"`
}

func (s *Server) retractDatapoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in retractInput
	if err := decodeJSON(r, body, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var probs problems
	probs.text("reason", strings.TrimSpace(in.Reason), 1, 2000)
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r.Context())
	s.mutate(w, r, body, func(tx pgx.Tx) (txResult, error) {
		before, err := s.reader.Get(r.Context(), id)
		if err != nil {
			return txResult{}, err
		}
		uid := p.UserID
		if err := s.writer.Retract(r.Context(), tx, datapoints.WriteMeta{
			Source: catalog.SourceKeyAnalyst, ActorID: &uid, Origin: datapoints.OriginPrimary,
		}, []int64{id}, in.Reason); err != nil {
			return txResult{}, err
		}
		prev := map[string]any{"series": before.SeriesKey, "obs_date": before.ObsDate, "value": valueOut(before.Value), "source": before.Source}
		if err := s.audit.Record(r.Context(), tx, auditEntry(r, "datapoint", strconv.FormatInt(id, 10), "retract", prev, map[string]any{"reason": in.Reason})); err != nil {
			return txResult{}, err
		}
		return txResult{Status: http.StatusOK, Body: map[string]any{"id": id, "status": "retracted"}}, nil
	})
}

// ---------------------------------------------------------------- snapshot

type snapshotInput struct {
	AsOf   string   `json:"as_of"`
	Cutoff string   `json:"cutoff"`
	Series []string `json:"series"`
}

type snapshotValueJSON struct {
	Value       any    `json:"value"`
	DatapointID int64  `json:"datapoint_id"`
	Source      string `json:"source"`
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	var in snapshotInput
	if err := decodeBody(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	var probs problems
	asOf, derr := civil.ParseDate(in.AsOf)
	if derr != nil || !asOf.IsValid() {
		probs.add("as_of", "invalid_date")
	}
	now := s.clock.Now()
	cutoff := now
	if in.Cutoff != "" {
		t, err := time.Parse(time.RFC3339, in.Cutoff)
		if err != nil {
			probs.add("cutoff", "invalid_timestamp")
		} else if t.Before(now) {
			cutoff = t.UTC()
		}
	}
	if len(in.Series) < 1 || len(in.Series) > 500 || !unique(in.Series) {
		probs.add("series", "invalid_length")
	}
	for i, k := range in.Series {
		if !keyPatternRE.MatchString(k) {
			probs.addAt(i, "series", "invalid_key")
		}
	}
	if err := probs.err(); err != nil {
		s.writeError(w, r, err)
		return
	}

	keys, missing, err := s.expandSeries(r.Context(), in.Series)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	got, err := s.reader.Snapshot(r.Context(), keys, asOf, datapoints.ReadOpts{Cutoff: cutoff})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	values := make(map[string]snapshotValueJSON, len(got))
	for k, rv := range got {
		values[k] = snapshotValueJSON{Value: valueOut(rv.Value), DatapointID: rv.ID, Source: rv.Source}
	}
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	missing = append([]string{}, dedupe(missing)...)
	writeJSON(w, http.StatusOK, map[string]any{"as_of": asOf, "values": values, "missing": missing})
}

// expandSeries resolves exact keys and trailing-* patterns to known series.
// Exact keys that do not exist are returned as missing.
func (s *Server) expandSeries(ctx context.Context, patterns []string) (keys, missing []string, err error) {
	seen := map[string]bool{}
	var exact []string
	var wild []string
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			wild = append(wild, p)
		} else {
			exact = append(exact, p)
		}
	}
	for _, k := range exact {
		if _, err := s.catalog.Series(ctx, k); err != nil {
			if apperr.CodeOf(toAppErr(err)) == apperr.NotFound {
				missing = append(missing, k)
				continue
			}
			return nil, nil, err
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(wild) > 0 {
		req := paging.Request{Limit: paging.MaxLimit}
		for {
			page, err := s.catalog.ListSeries(ctx, catalog.SeriesFilter{Keys: wild, Page: req})
			if err != nil {
				return nil, nil, err
			}
			for _, ser := range page.Items {
				if !seen[ser.Key] {
					seen[ser.Key] = true
					keys = append(keys, ser.Key)
				}
			}
			if len(keys) > maxSnapshotSeries {
				return nil, nil, apperr.New(apperr.InvalidParams, "Patterns match too many series", apperr.Detail{Field: "series", Reason: "too_many_series"})
			}
			if page.Next == "" {
				break
			}
			req.After = page.Next
		}
	}
	sort.Strings(keys)
	return keys, missing, nil
}
