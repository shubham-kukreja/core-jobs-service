package datapoints

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
)

// MaxBatch bounds the points (or ids) accepted by one Write or Retract.
const MaxBatch = 10_000

// maxKnownAtSkew tolerates small clock differences between writers.
const maxKnownAtSkew = time.Minute

// Rejection reasons.
const (
	ReasonUnknownSeries    = "unknown_series"
	ReasonInactiveSeries   = "inactive_series"
	ReasonWrongType        = "wrong_type"
	ReasonOutOfRange       = "out_of_range"
	ReasonNotAllowed       = "not_allowed"
	ReasonNotWritable      = "not_writable"
	ReasonInvalidDate      = "invalid_date"
	ReasonInvalidKnownAt   = "invalid_known_at"
	ReasonDuplicatePoint   = "duplicate_point"
	ReasonDuplicateKnownAt = "duplicate_known_at"
	ReasonNoteTooLong      = "note_too_long"
)

const maxNoteLen = 2000

// priorityIDs expands a source priority list into ordered source ids. Each
// entry names a source key or a source kind (all sources of that kind, by
// key); the first mention of a source wins.
func priorityIDs(entries []string, sources []catalog.Source) []int64 {
	byKey := map[string]catalog.Source{}
	byKind := map[catalog.SourceKind][]catalog.Source{}
	for _, s := range sources {
		byKey[s.Key] = s
		byKind[s.Kind] = append(byKind[s.Kind], s)
	}
	for k := range byKind {
		sort.Slice(byKind[k], func(i, j int) bool { return byKind[k][i].Key < byKind[k][j].Key })
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, e := range entries {
		if s, ok := byKey[e]; ok {
			add(s.ID)
			continue
		}
		for _, s := range byKind[catalog.SourceKind(e)] {
			add(s.ID)
		}
	}
	return out
}

// writable reports whether a source of kind k may write series of layer l.
// Computed layers are only ever written by the system; analysts and imports
// write observations and judgments; overrides (admin) may correct anything.
func writable(k catalog.SourceKind, l catalog.Layer) bool {
	switch k {
	case catalog.SourceOverride:
		return true
	case catalog.SourceComputed:
		return l.Computed() || l == catalog.LayerTrigger || l == catalog.LayerVeto
	case catalog.SourceAnalyst:
		return l == catalog.LayerRaw || l == catalog.LayerJudgment || l == catalog.LayerTrigger
	case catalog.SourceFileImport, catalog.SourceExternalAPI:
		return l == catalog.LayerRaw
	}
	return false
}

// checkPoint validates one point against its series and returns a rejection
// reason and message, or "" when the point is acceptable.
func checkPoint(s catalog.Series, src catalog.Source, p Point, now time.Time) (reason, msg string) {
	if !s.Active {
		return ReasonInactiveSeries, "series is inactive"
	}
	if !writable(src.Kind, s.Layer) {
		return ReasonNotWritable, fmt.Sprintf("%s series cannot be written by source %q", s.Layer, src.Key)
	}
	if !p.ObsDate.IsValid() {
		return ReasonInvalidDate, "obs_date is not a valid date"
	}
	if !p.KnownAt.IsZero() && p.KnownAt.After(now.Add(maxKnownAtSkew)) {
		return ReasonInvalidKnownAt, "known_at is in the future"
	}
	if len(p.Note) > maxNoteLen {
		return ReasonNoteTooLong, fmt.Sprintf("note exceeds %d characters", maxNoteLen)
	}
	v := p.Value
	set := 0
	for _, b := range []bool{v.Num != nil, v.Bool != nil, v.Cat != nil} {
		if b {
			set++
		}
	}
	switch {
	case set > 1:
		return ReasonWrongType, "a value must be exactly one of number, boolean or category"
	case set == 0:
		// Null is how computations record "not computable" (e.g. below min_obs).
		if src.Kind != catalog.SourceComputed {
			return ReasonWrongType, "value is required"
		}
		return "", ""
	}
	switch s.ValueType {
	case catalog.ValueNum:
		if v.Num == nil {
			return ReasonWrongType, "series expects a number"
		}
		if math.IsNaN(*v.Num) || math.IsInf(*v.Num, 0) {
			return ReasonWrongType, "number must be finite"
		}
		if (s.Validation.Min != nil && *v.Num < *s.Validation.Min) || (s.Validation.Max != nil && *v.Num > *s.Validation.Max) {
			return ReasonOutOfRange, "value is outside the series' allowed range"
		}
	case catalog.ValueBool:
		if v.Bool == nil {
			return ReasonWrongType, "series expects a boolean"
		}
	case catalog.ValueCat:
		if v.Cat == nil {
			return ReasonWrongType, "series expects a category"
		}
		if !slices.Contains(s.AllowedValues, *v.Cat) {
			return ReasonNotAllowed, "category is not one of the series' allowed values"
		}
	}
	return "", ""
}

// inputHash identifies a computed value's inputs: the derivation version and
// the sorted input datapoint ids. Equal hashes mean the result is unchanged.
func inputHash(derivVer int, inputs []int64) []byte {
	ids := slices.Clone(inputs)
	slices.Sort(ids)
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(derivVer)) //nolint:gosec // versions are small positive ints.
	h.Write(buf[:])
	for _, id := range ids {
		binary.BigEndian.PutUint64(buf[:], uint64(id)) //nolint:gosec // ids are positive identities.
		h.Write(buf[:])
	}
	return h.Sum(nil)
}

// sameValue reports whether two values are identical (including both null).
func sameValue(a, b Value) bool {
	eq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqB := func(x, y *bool) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqS := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.Num, b.Num) && eqB(a.Bool, b.Bool) && eqS(a.Cat, b.Cat)
}
