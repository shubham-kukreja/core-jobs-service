package jobs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/civil"
)

// ResolveArgTemplates replaces template strings in schedule args with dates
// derived from slot in loc:
//
//	$today           slot's date
//	$slot            slot's date (review runs: the review date itself)
//	$month_end       last day of slot's month
//	$prev_month_end  last day of the month before slot's
//
// Any other string starting with "$" is rejected, so typos fail at save time.
func ResolveArgTemplates(raw json.RawMessage, slot time.Time, loc *time.Location) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("schedule args: %w", err)
	}
	d := civil.DateOf(slot.In(loc))
	resolved, err := resolveValue(v, d)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resolved)
}

func resolveValue(v any, d civil.Date) (any, error) {
	switch x := v.(type) {
	case string:
		if !strings.HasPrefix(x, "$") {
			return x, nil
		}
		switch x {
		case "$today", "$slot":
			return d.String(), nil
		case "$month_end":
			return monthEnd(d).String(), nil
		case "$prev_month_end":
			return monthEnd(d.AddMonths(-1)).String(), nil
		}
		return nil, fmt.Errorf("schedule args: unknown template %q", x)
	case map[string]any:
		for k, e := range x {
			r, err := resolveValue(e, d)
			if err != nil {
				return nil, err
			}
			x[k] = r
		}
		return x, nil
	case []any:
		for i, e := range x {
			r, err := resolveValue(e, d)
			if err != nil {
				return nil, err
			}
			x[i] = r
		}
		return x, nil
	default:
		return v, nil
	}
}

func monthEnd(d civil.Date) civil.Date {
	first := civil.Date{Year: d.Year, Month: d.Month, Day: 1}
	return first.AddMonths(1).AddDays(-1)
}
