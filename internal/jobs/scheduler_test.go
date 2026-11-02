package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

var ist = mustLoc("Asia/Kolkata")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func TestCronInScheduleTimezone(t *testing.T) {
	// 08:30 IST on weekdays = 03:00 UTC.
	after := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC) // Fri 09:30 IST
	next, err := CronType{}.Next("30 8 * * 1-5", after, ist)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC) // Mon 08:30 IST
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	if err := (CronType{}).Validate("61 * * * *", ist); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

func TestInterval(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	next, err := IntervalType{}.Next("6h", at, ist)
	if err != nil || !next.Equal(at.Add(6*time.Hour)) {
		t.Fatalf("next = %s, %v", next, err)
	}
	for _, bad := range []string{"30s", "abc", "9000h"} {
		if (IntervalType{}).Validate(bad, ist) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDueSlotsAndCatchup(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	first := fc.Now().Add(-5*time.Hour - 30*time.Minute) // missed hourly slots

	due, next, err := dueSlots(IntervalType{}, "1h", first, fc.Now(), ist)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 6 || !next.After(fc.Now()) || next.Sub(due[len(due)-1]) != time.Hour {
		t.Fatalf("due=%d next=%s", len(due), next)
	}

	if got := applyCatchup(CatchupAll, due, fc.Now()); len(got) != 6 {
		t.Errorf("all: %d slots", len(got))
	}
	if got := applyCatchup(CatchupLatest, due, fc.Now()); len(got) != 1 || !got[0].Equal(due[5]) {
		t.Errorf("latest: %v", got)
	}
	// Last slot was 30 minutes ago: beyond the grace window, so none fires.
	if got := applyCatchup(CatchupNone, due, fc.Now()); len(got) != 0 {
		t.Errorf("none (late): %v", got)
	}
	fc.Set(due[5].Add(time.Minute))
	if got := applyCatchup(CatchupNone, due, fc.Now()); len(got) != 1 {
		t.Errorf("none (on time): %v", got)
	}
}

func TestCatchupAllIsCapped(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	due, _, err := dueSlots(IntervalType{}, "1m", now.Add(-1000*time.Minute), now, ist)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != MaxCatchup || !due[len(due)-1].Equal(now) {
		t.Fatalf("len=%d last=%s", len(due), due[len(due)-1])
	}
}

// No double fire: once a tick advances next_run_at past now, the next tick
// at the same instant finds nothing due.
func TestNoDoubleFire(t *testing.T) {
	fc := clock.NewFake(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	first := fc.Now()
	due, next, err := dueSlots(CronType{}, "30 8 * * *", first, fc.Now(), ist)
	if err != nil || len(due) != 1 {
		t.Fatalf("first tick: due=%v err=%v", due, err)
	}
	again, _, _ := dueSlots(CronType{}, "30 8 * * *", next, fc.Now(), ist)
	if len(again) != 0 {
		t.Fatalf("second tick fired again: %v", again)
	}
	fc.Advance(24 * time.Hour)
	later, _, _ := dueSlots(CronType{}, "30 8 * * *", next, fc.Now(), ist)
	if len(later) != 1 || !later[0].Equal(next) {
		t.Fatalf("next day: %v", later)
	}
}

func TestResolveArgTemplates(t *testing.T) {
	// 2026-12-01 00:30 IST is still 2026-11-30 in UTC: dates use the schedule tz.
	slot := time.Date(2026, 11, 30, 19, 0, 0, 0, time.UTC)
	got, err := ResolveArgTemplates(json.RawMessage(
		`{"as_of":"$today","slot":"$slot","me":"$month_end","pme":"$prev_month_end","checks":["stale"],"n":2}`), slot, ist)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	want := map[string]any{"as_of": "2026-12-01", "slot": "2026-12-01", "me": "2026-12-31", "pme": "2026-11-30"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if m["n"] != float64(2) || m["checks"].([]any)[0] != "stale" {
		t.Errorf("non-template values changed: %v", m)
	}
	if _, err := ResolveArgTemplates(json.RawMessage(`{"as_of":"$yesterday"}`), slot, ist); err == nil {
		t.Fatal("unknown template accepted")
	}
}

func TestJitterDeterministicAndBounded(t *testing.T) {
	slot := time.Date(2026, 11, 14, 3, 30, 0, 0, time.UTC)
	a, b := jitter(1, slot, 120), jitter(1, slot, 120)
	if a != b || a < 0 || a > 120*time.Second {
		t.Fatalf("jitter %s %s", a, b)
	}
	if jitter(1, slot, 0) != 0 {
		t.Fatal("zero jitter must be zero")
	}
}

func TestScheduleTypesRegistry(t *testing.T) {
	r := DefaultScheduleTypes()
	if _, ok := r.Get("cron"); !ok {
		t.Fatal("cron missing")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate type must panic")
		}
	}()
	r.Register(IntervalType{})
}

// TestPrevMonthEndOnLongMonthDays guards the month-overflow bug: on 31 Mar,
// "one month back" must be February, not 3 Mar after normalisation.
func TestPrevMonthEndOnLongMonthDays(t *testing.T) {
	for slot, want := range map[time.Time]string{
		time.Date(2026, 3, 31, 6, 0, 0, 0, time.UTC): "2026-02-28",
		time.Date(2024, 3, 30, 6, 0, 0, 0, time.UTC): "2024-02-29",
		time.Date(2026, 5, 31, 6, 0, 0, 0, time.UTC): "2026-04-30",
		time.Date(2026, 1, 31, 6, 0, 0, 0, time.UTC): "2025-12-31",
	} {
		got, err := ResolveArgTemplates(json.RawMessage(`{"as_of":"$prev_month_end"}`), slot, ist)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		_ = json.Unmarshal(got, &m)
		if m["as_of"] != want {
			t.Errorf("%s: prev_month_end = %s, want %s", slot.Format(time.DateOnly), m["as_of"], want)
		}
	}
}
