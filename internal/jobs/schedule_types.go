package jobs

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduleTypes is the registry of schedule expression types. Calendar types
// (business_day:N, month_end+Nbd, review:…) register here in Phase 2.
type ScheduleTypes struct {
	mu    sync.RWMutex
	types map[string]ScheduleType
}

// NewScheduleTypes returns a registry holding ts.
func NewScheduleTypes(ts ...ScheduleType) *ScheduleTypes {
	r := &ScheduleTypes{types: map[string]ScheduleType{}}
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

// DefaultScheduleTypes holds cron and interval.
func DefaultScheduleTypes() *ScheduleTypes { return NewScheduleTypes(CronType{}, IntervalType{}) }

// Register adds a type; a duplicate name panics at boot.
func (r *ScheduleTypes) Register(t ScheduleType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.types[t.Name()]; dup {
		panic(fmt.Sprintf("jobs: duplicate schedule type %q", t.Name()))
	}
	r.types[t.Name()] = t
}

// Get returns a type by name.
func (r *ScheduleTypes) Get(name string) (ScheduleType, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.types[name]
	return t, ok
}

// Names lists registered types.
func (r *ScheduleTypes) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.types))
	for n := range r.types {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CronType is standard 5-field cron evaluated in the schedule's timezone.
type CronType struct{}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func (CronType) Name() string { return "cron" }

func (CronType) Validate(expr string, _ *time.Location) error {
	if _, err := cronParser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}

func (CronType) Next(expr string, after time.Time, loc *time.Location) (time.Time, error) {
	s, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	next := s.Next(after.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron %q never fires", expr)
	}
	return next.UTC(), nil
}

// IntervalType fires every fixed duration, e.g. "6h".
type IntervalType struct{}

const (
	minInterval = time.Minute
	maxInterval = 366 * 24 * time.Hour
)

func (IntervalType) Name() string { return "interval" }

func (IntervalType) Validate(expr string, _ *time.Location) error {
	_, err := parseInterval(expr)
	return err
}

func (IntervalType) Next(expr string, after time.Time, _ *time.Location) (time.Time, error) {
	d, err := parseInterval(expr)
	if err != nil {
		return time.Time{}, err
	}
	return after.Add(d).UTC(), nil
}

func parseInterval(expr string) (time.Duration, error) {
	d, err := time.ParseDuration(expr)
	if err != nil {
		return 0, fmt.Errorf("invalid interval: %w", err)
	}
	if d < minInterval || d > maxInterval {
		return 0, fmt.Errorf("interval must be between %s and %s", minInterval, maxInterval)
	}
	return d, nil
}
