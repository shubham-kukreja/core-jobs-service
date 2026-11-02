package calendar

import (
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

func d(y, m, day int) civil.Date { return civil.Date{Year: y, Month: time.Month(m), Day: day} }

func TestAddMonths(t *testing.T) {
	cases := []struct {
		in   civil.Date
		n    int
		want civil.Date
	}{
		{d(2026, 5, 31), -3, d(2026, 2, 28)}, // clamp, not 3 Mar
		{d(2026, 3, 31), -1, d(2026, 2, 28)},
		{d(2024, 3, 31), -1, d(2024, 2, 29)}, // leap year
		{d(2026, 4, 30), 1, d(2026, 5, 31)},  // month end stays month end
		{d(2026, 1, 15), -1, d(2025, 12, 15)},
		{d(2026, 1, 31), -13, d(2024, 12, 31)},
		{d(2026, 12, 15), 1, d(2027, 1, 15)},
		{d(2026, 6, 30), -180, d(2011, 6, 30)},
	}
	for _, c := range cases {
		if got := AddMonths(c.in, c.n); got != c.want {
			t.Errorf("AddMonths(%s, %d) = %s, want %s", c.in, c.n, got, c.want)
		}
	}
}

func TestWindowStartHoldsExactlyNMonthEnds(t *testing.T) {
	if got := WindowStart(d(2026, 6, 30), 3); got != d(2026, 4, 1) {
		t.Fatalf("got %s, want 2026-04-01", got)
	}
	if got := WindowStart(d(2026, 6, 15), 3); got != d(2026, 3, 16) {
		t.Fatalf("got %s, want 2026-03-16", got)
	}
}

func TestMonthEnd(t *testing.T) {
	if got := MonthEnd(d(2024, 2, 3)); got != d(2024, 2, 29) || !IsMonthEnd(got) || IsMonthEnd(d(2024, 2, 28)) {
		t.Fatalf("got %s", got)
	}
}
