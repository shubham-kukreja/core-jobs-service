package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

type qcArgs struct {
	AsOf   civil.Date `json:"as_of"`
	Checks []string   `json:"checks,omitempty"`
}

func (qcArgs) Kind() string { return "quality.check" }

func scheduleStore() *ScheduleStore {
	r := NewRegistry()
	Register(r, Spec{
		Kind: "quality.check", Queue: QueueCompute, Triggers: []TriggerType{TriggerSchedule, TriggerUser},
		UserTriggerable: true, Roles: []string{"analyst"}, MaxAttempts: 3, Timeout: time.Minute, ArgsVersion: 1,
	}, HandlerFunc[qcArgs](func(context.Context, *RunCtx, qcArgs) (Outcome, error) { return Outcome{}, nil }))
	Register(r, pingSpec(), HandlerFunc[pingArgs](func(context.Context, *RunCtx, pingArgs) (Outcome, error) { return Outcome{}, nil }))
	clk := clock.NewFake(time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC))
	return NewScheduleStore(nil, r, DefaultScheduleTypes(), nil, clk)
}

func validSchedule() Schedule {
	return withScheduleDefaults(Schedule{
		Name: "quality-daily", Kind: "quality.check", ScheduleType: "cron", Expr: "30 8 * * 1-5",
		Args: json.RawMessage(`{"as_of":"$today"}`), Enabled: true,
	})
}

func TestValidateSchedule(t *testing.T) {
	s := scheduleStore()
	next, err := s.validate(validSchedule(), s.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Mon 2 Nov 09:30 IST → next weekday 08:30 IST is Tue 3 Nov 03:00 UTC.
	if want := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestValidateScheduleRejects(t *testing.T) {
	s := scheduleStore()
	cases := map[string]struct {
		mut   func(*Schedule)
		field string
	}{
		"bad name":        {func(x *Schedule) { x.Name = "Quality Daily" }, "name"},
		"unknown kind":    {func(x *Schedule) { x.Kind = "nope" }, "kind"},
		"not schedulable": {func(x *Schedule) { x.Kind = "test.ping"; x.Args = json.RawMessage(`{}`) }, "kind"},
		"bad expr":        {func(x *Schedule) { x.Expr = "every day" }, "expr"},
		"bad type":        {func(x *Schedule) { x.ScheduleType = "lunar" }, "schedule_type"},
		"bad tz":          {func(x *Schedule) { x.Timezone = "Mars/Olympus" }, "timezone"},
		"bad policy":      {func(x *Schedule) { x.OverlapPolicy = "maybe" }, "overlap_policy"},
		"jitter":          {func(x *Schedule) { x.JitterSecs = 7200 }, "jitter_secs"},
		"unknown field":   {func(x *Schedule) { x.Args = json.RawMessage(`{"as_of":"$today","oops":1}`) }, "args"},
		"bad template":    {func(x *Schedule) { x.Args = json.RawMessage(`{"as_of":"$tomorrow"}`) }, "args"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := validSchedule()
			tc.mut(&in)
			_, err := s.validate(in, s.clock.Now())
			ae, ok := apperr.As(err)
			if !ok || ae.Code != apperr.ValidationFailed {
				t.Fatalf("err = %v", err)
			}
			found := false
			for _, d := range ae.Details {
				found = found || d.Field == tc.field
			}
			if !found {
				t.Fatalf("no detail for %s in %+v", tc.field, ae.Details)
			}
		})
	}
}
