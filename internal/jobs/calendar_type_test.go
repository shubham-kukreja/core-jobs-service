package jobs

import (
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

func cd(y int, m time.Month, d int) civil.Date { return civil.Date{Year: y, Month: m, Day: d} }

// testHolidays is a sample exchange calendar for the tests, not the
// authoritative NSE list.
var testHolidays = HolidaySet{
	cd(2026, 1, 26): true, // Republic Day (Monday)
	cd(2026, 3, 3):  true,
	cd(2026, 10, 2): true, // Gandhi Jayanti (Friday)
}

func TestCalendarNext(t *testing.T) {
	c := CalendarType{Holidays: testHolidays, Reviews: func(cycle string) []civil.Date {
		if cycle == "quarterly" {
			return []civil.Date{cd(2026, 12, 31), cd(2027, 3, 31)}
		}
		return nil
	}}
	at := func(y int, m time.Month, d, hh, mm int) time.Time { return time.Date(y, m, d, hh, mm, 0, 0, ist) }
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		// Jan 2026: 1,2 | 5-9 | 12-16 → 12th business day = 16 Jan.
		{"business_day:12", at(2026, 1, 1, 0, 0), at(2026, 1, 16, 7, 0)},
		// Already past this month's fire → next month (Feb: 2-6, 9-13, 16, 17 → 17 Feb).
		{"business_day:12", at(2026, 1, 16, 7, 0), at(2026, 2, 17, 7, 0)},
		// March holiday on the 3rd shifts the count: 2,4,5 → 3rd bd = 5 Mar.
		{"business_day:3", at(2026, 3, 1, 0, 0), at(2026, 3, 5, 7, 0)},
		// 26 Jan is a holiday Monday → next business day.
		{"month_day:26", at(2026, 1, 1, 0, 0), at(2026, 1, 27, 7, 0)},
		// 31 in a 30-day month clamps to 30 Sep 2026 (Wednesday).
		{"month_day:31@09:30", at(2026, 9, 1, 0, 0), at(2026, 9, 30, 9, 30)},
		// After Sep 30 2026 (Wed): Oct 1 Thu, Oct 2 holiday, Oct 5 Mon → 3rd bd = Oct 6.
		{"month_end+3bd", at(2026, 10, 1, 0, 0), at(2026, 10, 6, 7, 0)},
		{"review:quarterly", at(2026, 12, 31, 7, 0), at(2027, 3, 31, 7, 0)},
	}
	for _, tc := range cases {
		if err := c.Validate(tc.expr, ist); err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		got, err := c.Next(tc.expr, tc.after, ist)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%s after %v: got %v (%v), want %v", tc.expr, tc.after, got.In(ist), err, tc.want)
		}
	}
	if _, err := c.Next("review:semiannual", at(2026, 1, 1, 0, 0), ist); err == nil {
		t.Error("review with no dates must not fire")
	}
	for _, bad := range []string{"business_day:0", "business_day:x", "month_day:32", "month_end+bd", "review:", "weekly", "business_day:1@25:00"} {
		if err := c.Validate(bad, ist); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCalendarNoDoubleFire(t *testing.T) {
	c := CalendarType{Holidays: testHolidays}
	t1, err := c.Next("business_day:10", time.Date(2026, 1, 1, 0, 0, 0, 0, ist), ist)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := c.Next("business_day:10", t1, ist)
	if err != nil || !t2.After(t1) || t2.Month() == t1.Month() {
		t.Fatalf("t1 %v t2 %v %v", t1, t2, err)
	}
}
