// Package calendar does month arithmetic on civil dates without the
// overflow normalisation of time.AddDate (31 May − 3 months must be 28 Feb,
// not 3 Mar). Every look-back window and month-end template uses it.
package calendar

import (
	"time"

	"cloud.google.com/go/civil"
)

// DaysIn returns the number of days in the given month.
func DaysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// MonthEnd returns the last day of d's month.
func MonthEnd(d civil.Date) civil.Date {
	return civil.Date{Year: d.Year, Month: d.Month, Day: DaysIn(d.Year, d.Month)}
}

// IsMonthEnd reports whether d is the last day of its month.
func IsMonthEnd(d civil.Date) bool { return d.Day == DaysIn(d.Year, d.Month) }

// AddMonths moves d by n months, clamping the day to the target month's
// length. A month-end date stays a month end (30 Apr + 1 month = 31 May).
func AddMonths(d civil.Date, n int) civil.Date {
	total := int(d.Month) - 1 + n
	year := d.Year + floorDiv(total, 12)
	month := time.Month(total - floorDiv(total, 12)*12 + 1)
	day := d.Day
	if last := DaysIn(year, month); day > last || IsMonthEnd(d) {
		day = last
	}
	return civil.Date{Year: year, Month: month, Day: day}
}

// WindowStart returns the first date of the n-month window ending at upTo,
// i.e. the window is [WindowStart(upTo, n), upTo] and holds exactly n
// month-ends when upTo is a month end.
func WindowStart(upTo civil.Date, n int) civil.Date {
	return AddMonths(upTo, -n).AddDays(1)
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
