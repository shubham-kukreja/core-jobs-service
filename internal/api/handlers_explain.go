package api

import (
	"net/http"
	"time"

	"cloud.google.com/go/civil"
	"github.com/go-chi/chi/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

type explainInputJSON struct {
	Key      string `json:"key"`
	Label    string `json:"label,omitempty"`
	Value    any    `json:"value"`
	Unit     string `json:"unit,omitempty"`
	Source   string `json:"source,omitempty"`
	Observed string `json:"observed,omitempty"`
}

type explainPartJSON struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Result    *float64 `json:"result,omitempty"`
	Direction string   `json:"direction"`
	Steps     []string `json:"steps"`
}

type explanationJSON struct {
	Key       string             `json:"key"`
	Score     *float64           `json:"score"`
	Sources   []string           `json:"sources"`
	Observed  *string            `json:"observed"`
	Freshness string             `json:"freshness"`
	Formula   string             `json:"formula"`
	Inputs    []explainInputJSON `json:"inputs"`
	Parts     []explainPartJSON  `json:"parts"`
	Final     string             `json:"final,omitempty"`
	Notes     []string           `json:"notes"`
}

// explainSeries serves GET /v1/series/{key}/explain: the generated formula
// breakdown of the series' value at obs_date (default: latest), as known at
// the cutoff.
func (s *Server) explainSeries(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !keyRE.MatchString(key) {
		s.writeError(w, r, apperr.New(apperr.NotFound, "Not found"))
		return
	}
	var d civil.Date
	if v := r.URL.Query().Get("obs_date"); v != "" {
		var err error
		if d, err = parseDate("obs_date", v); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	now := s.clock.Now()
	cutoff, _, err := queryCutoff(r, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ex, err := s.explainer.Explain(r.Context(), key, d, datapoints.ReadOpts{Cutoff: cutoff})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	names := map[string]string{}
	if srcs, err := s.catalog.ListSources(r.Context()); err == nil {
		for _, src := range srcs {
			names[src.Key] = src.Name
		}
	}
	writeJSON(w, http.StatusOK, toExplanationJSON(ex, cutoff, names))
}

func toExplanationJSON(ex derive.Explanation, now time.Time, sourceNames map[string]string) explanationJSON {
	srcName := func(k string) string {
		if n := sourceNames[k]; n != "" {
			return n
		}
		return k
	}
	out := explanationJSON{
		Key: ex.Series.Key, Formula: ex.Formula, Freshness: freshness(ex.Series, ex.Point, now),
		Sources: []string{}, Inputs: []explainInputJSON{}, Parts: []explainPartJSON{}, Notes: []string{},
	}
	if ex.Formula == "" {
		out.Formula = "Recorded value (no formula)"
	}
	if ex.Point != nil {
		out.Score = ex.Point.Value.Num
		o := ex.Point.ObsDate.String()
		out.Observed = &o
	}

	// Inputs are the leaves of the tree: raw readings, analyst entries and
	// anything not expanded further. A raw series is its own leaf.
	seenSrc := map[string]bool{}
	addSource := func(k string) {
		if k != "" && !seenSrc[k] {
			seenSrc[k] = true
			out.Sources = append(out.Sources, srcName(k))
		}
	}
	if ex.Series.Derivation == nil && ex.Point != nil {
		addSource(ex.Point.Source)
	}
	seenLeaf := map[string]bool{}
	var leaves func(e derive.Explanation)
	leaves = func(e derive.Explanation) {
		for _, in := range e.Inputs {
			if in.Expanded {
				continue
			}
			id := in.Series.Key
			if in.Point != nil {
				id += "@" + in.Point.ObsDate.String()
			}
			if seenLeaf[id] {
				continue
			}
			seenLeaf[id] = true
			lj := explainInputJSON{Key: in.Series.Key, Label: label(in.Series), Unit: in.Series.Unit}
			if in.Point != nil {
				lj.Value = valueOut(in.Point.Value)
				lj.Source = srcName(in.Point.Source)
				lj.Observed = in.Point.ObsDate.String()
				addSource(in.Point.Source)
			}
			out.Inputs = append(out.Inputs, lj)
		}
		for _, p := range e.Parts {
			leaves(p)
		}
	}
	leaves(ex)

	// The last step is the final line; earlier top-level steps (a
	// percentile's window, inputs an average left out) read as notes.
	if n := len(ex.Steps); n > 0 {
		out.Final = ex.Steps[n-1].Text
		for _, st := range ex.Steps[:n-1] {
			out.Notes = append(out.Notes, st.Text)
		}
	}
	out.Notes = append(out.Notes, ex.Notes...)
	for _, p := range ex.Parts {
		pj := explainPartJSON{ID: p.Alias, Title: label(p.Series), Direction: "as_is", Steps: partSteps(p, "")}
		if p.Series.Derivation != nil && p.Series.Derivation.Fn == "invert" {
			pj.Direction = "inverted"
		}
		if p.Point != nil {
			pj.Result = p.Point.Value.Num
		} else if n := len(p.Steps); n > 0 {
			pj.Result = p.Steps[n-1].Result
		}
		out.Parts = append(out.Parts, pj)
		var notes func(e derive.Explanation)
		notes = func(e derive.Explanation) {
			for _, n := range e.Notes {
				out.Notes = append(out.Notes, label(e.Series)+": "+n)
			}
			for _, c := range e.Parts {
				notes(c)
			}
		}
		notes(p)
	}
	return out
}

// partSteps flattens a part's breakdown, deepest first, so it reads in the
// order the numbers were computed. Sub-steps carry their series' label.
func partSteps(e derive.Explanation, prefix string) []string {
	var out []string
	for _, c := range e.Parts {
		out = append(out, partSteps(c, label(c.Series)+": ")...)
	}
	for _, st := range e.Steps {
		out = append(out, prefix+st.Text)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func label(s catalog.Series) string {
	if s.Registry.DisplayName != "" {
		return s.Registry.DisplayName
	}
	if s.Name != "" {
		return s.Name
	}
	return s.Key
}

// Default staleness when a series sets no stale_after: a little over one
// period, so a value is stale once the next one is overdue.
var defaultStaleAfter = map[catalog.Frequency]time.Duration{
	catalog.FreqDaily:     4 * 24 * time.Hour,
	catalog.FreqWeekly:    10 * 24 * time.Hour,
	catalog.FreqMonthly:   40 * 24 * time.Hour,
	catalog.FreqQuarterly: 100 * 24 * time.Hour,
}

// freshness is missing without a value, stale once the observation is older
// than the series' stale_after at now, else fresh. Event series never go stale.
func freshness(s catalog.Series, p *datapoints.Resolved, now time.Time) string {
	if p == nil {
		return "missing"
	}
	after := s.Registry.StaleAfter
	if after == 0 {
		after = defaultStaleAfter[s.Frequency]
	}
	obs := p.ObsDate.In(time.UTC)
	if after > 0 && now.Sub(obs) > after {
		return "stale"
	}
	return "fresh"
}
