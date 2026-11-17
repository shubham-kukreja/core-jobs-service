package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

// HolidaySet is an exchange holiday list. Weekends are always closed.
type HolidaySet map[civil.Date]bool

// IsBusinessDay reports whether d is a weekday and not a holiday.
func (h HolidaySet) IsBusinessDay(d civil.Date) bool {
	wd := d.In(time.UTC).Weekday()
	return wd != time.Saturday && wd != time.Sunday && !h[d]
}

// CalendarType fires on business-day calendars and review dates:
//
//	business_day:10      10th business day of each month
//	month_day:12         12th of the month, or the next business day
//	month_end+3bd        3rd business day after the previous month's end
//	review:quarterly     each date of the model config's review_calendar
//
// Any expression may end in "@HH:MM" (schedule timezone); the default is 07:00.
type CalendarType struct {
	Holidays HolidaySet
	// Reviews returns a review cycle's dates; nil or empty means none
	// scheduled yet, and the schedule does not fire.
	Reviews func(cycle string) []civil.Date
}

func (CalendarType) Name() string { return "calendar" }

type calExpr struct {
	kind   string // business_day | month_day | month_end | review
	n      int
	cycle  string
	hh, mm int
}

func parseCalendar(expr string) (calExpr, error) {
	e := calExpr{hh: 7}
	body, at, hasAt := strings.Cut(strings.TrimSpace(expr), "@")
	if hasAt {
		t, err := time.Parse("15:04", at)
		if err != nil {
			return e, fmt.Errorf("invalid time %q in %q (want HH:MM)", at, expr)
		}
		e.hh, e.mm = t.Hour(), t.Minute()
	}
	num := func(s string, lo, hi int) (int, error) {
		n, err := strconv.Atoi(s)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("invalid calendar expression %q: number must be %d..%d", expr, lo, hi)
		}
		return n, nil
	}
	var err error
	switch {
	case strings.HasPrefix(body, "business_day:"):
		e.kind = "business_day"
		e.n, err = num(strings.TrimPrefix(body, "business_day:"), 1, 23)
	case strings.HasPrefix(body, "month_day:"):
		e.kind = "month_day"
		e.n, err = num(strings.TrimPrefix(body, "month_day:"), 1, 31)
	case strings.HasPrefix(body, "month_end+") && strings.HasSuffix(body, "bd"):
		e.kind = "month_end"
		e.n, err = num(strings.TrimSuffix(strings.TrimPrefix(body, "month_end+"), "bd"), 1, 23)
	case strings.HasPrefix(body, "review:"):
		e.kind = "review"
		e.cycle = strings.TrimPrefix(body, "review:")
		if e.cycle == "" {
			err = fmt.Errorf("invalid calendar expression %q: review needs a cycle", expr)
		}
	default:
		err = fmt.Errorf("invalid calendar expression %q", expr)
	}
	return e, err
}

func (CalendarType) Validate(expr string, _ *time.Location) error {
	_, err := parseCalendar(expr)
	return err
}

// Next returns the first fire time strictly after `after`.
func (c CalendarType) Next(expr string, after time.Time, loc *time.Location) (time.Time, error) {
	e, err := parseCalendar(expr)
	if err != nil {
		return time.Time{}, err
	}
	local := after.In(loc)
	fire := func(d civil.Date) time.Time {
		return time.Date(d.Year, d.Month, d.Day, e.hh, e.mm, 0, 0, loc)
	}
	if e.kind == "review" {
		if c.Reviews != nil {
			for _, d := range c.Reviews(e.cycle) { // sorted ascending by the model config
				if t := fire(d); t.After(after) {
					return t.UTC(), nil
				}
			}
		}
		return time.Time{}, fmt.Errorf("no upcoming %s review date", e.cycle)
	}
	month := civil.Date{Year: local.Year(), Month: local.Month(), Day: 1}
	for i := 0; i < 36; i++ { // three years covers any holiday-heavy stretch
		d, ok := c.dayIn(e, calendar.AddMonths(month, i))
		if ok {
			if t := fire(d); t.After(after) {
				return t.UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("calendar %q never fires", expr)
}

// dayIn returns the fire date of e for the month starting at first.
func (c CalendarType) dayIn(e calExpr, first civil.Date) (civil.Date, bool) {
	switch e.kind {
	case "business_day":
		return c.nthBusinessDay(first.AddDays(-1), e.n, first.Month)
	case "month_day":
		d := first.AddDays(min(e.n, calendar.DaysIn(first.Year, first.Month)) - 1)
		for !c.Holidays.IsBusinessDay(d) {
			d = d.AddDays(1)
		}
		return d, true
	case "month_end":
		return c.nthBusinessDay(first.AddDays(-1), e.n, 0)
	}
	return civil.Date{}, false
}

// nthBusinessDay returns the nth business day after base. When month is set
// the result must fall in that month.
func (c CalendarType) nthBusinessDay(base civil.Date, n int, month time.Month) (civil.Date, bool) {
	d := base
	for n > 0 {
		d = d.AddDays(1)
		if month != 0 && d.Month != month {
			return civil.Date{}, false
		}
		if c.Holidays.IsBusinessDay(d) {
			n--
		}
	}
	return d, true
}
