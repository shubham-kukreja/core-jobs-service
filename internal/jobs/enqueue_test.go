package jobs

import (
	"encoding/json"
	"testing"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

func TestCheckTrigger(t *testing.T) {
	uid := int64(7)
	sid := int64(3)
	user := pingSpec() // user-triggerable
	sched := pingSpec()
	sched.Triggers = []TriggerType{TriggerSchedule}
	sched.UserTriggerable = false

	cases := map[string]struct {
		spec    Spec
		opts    EnqueueOpts
		code    apperr.Code
		wantErr bool
	}{
		"user ok":               {user, EnqueueOpts{Trigger: TriggerUser, UserID: &uid}, "", false},
		"user missing id":       {user, EnqueueOpts{Trigger: TriggerUser}, "", true},
		"user not triggerable":  {sched, EnqueueOpts{Trigger: TriggerUser, UserID: &uid}, apperr.NotUserTriggerable, true},
		"schedule ok":           {sched, EnqueueOpts{Trigger: TriggerSchedule, ScheduleID: &sid}, "", false},
		"schedule missing id":   {sched, EnqueueOpts{Trigger: TriggerSchedule}, "", true},
		"schedule with user":    {sched, EnqueueOpts{Trigger: TriggerSchedule, ScheduleID: &sid, UserID: &uid}, "", true},
		"event not allowed":     {sched, EnqueueOpts{Trigger: TriggerEvent}, "", true},
		"chain always allowed":  {sched, EnqueueOpts{Trigger: TriggerChain}, "", false},
		"system always allowed": {user, EnqueueOpts{Trigger: TriggerSystem}, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkTrigger(tc.spec, tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.code != "" && apperr.CodeOf(err) != tc.code {
				t.Fatalf("code = %s, want %s", apperr.CodeOf(err), tc.code)
			}
		})
	}
}

func TestArgsSubset(t *testing.T) {
	got, err := argsSubset([]byte(`{"model":"maaf","as_of":"2026-10-31","publish":false}`), []string{"model", "as_of", "config"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	if m["model"] != "maaf" || m["as_of"] != "2026-10-31" || m["config"] != nil || len(m) != 3 {
		t.Fatalf("subset = %s", got)
	}
	if _, ok := m["publish"]; ok {
		t.Fatal("unselected field kept")
	}
}

func TestSameJSON(t *testing.T) {
	same, _ := sameJSON([]byte(`{"a":1,"b":[1,2]}`), []byte(`{"b":[1,2], "a":1.0}`))
	diff, _ := sameJSON([]byte(`{"a":1}`), []byte(`{"a":2}`))
	if !same || diff {
		t.Fatalf("same=%v diff=%v", same, diff)
	}
}

func TestLockKeyStable(t *testing.T) {
	first, again := lockKey("a", "b"), lockKey("a", "b")
	if first != again || first == lockKey("ab") {
		t.Fatal("lock keys must be stable and separator-aware")
	}
}
