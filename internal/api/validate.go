package api

import (
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Patterns mirror api/openapi.yaml and the SQL CHECKs.
var (
	keyRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-]{0,126}(::[a-z0-9][a-z0-9_\-]{0,62})?$`)
	keyPatternRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:\-]{0,190}\*?$`)
	tagRE        = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	metaKeyRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	aliasRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	fnRE         = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	categoryRE   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	staleRE      = regexp.MustCompile(`^([0-9]{1,4})(h|d)$`)
)

// problems collects validation details and turns them into one error.
type problems []apperr.Detail

func (p *problems) add(field, reason string) {
	*p = append(*p, apperr.Detail{Field: field, Reason: reason})
}

func (p *problems) addAt(i int, field, reason string) {
	idx := i
	*p = append(*p, apperr.Detail{Index: &idx, Field: field, Reason: reason})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return apperr.New(apperr.ValidationFailed, "Invalid request", p...)
}

func (p *problems) key(field, v string) {
	if !keyRE.MatchString(v) {
		p.add(field, "invalid_key")
	}
}

func (p *problems) text(field, v string, minLen, maxLen int) {
	if n := utf8.RuneCountInString(v); n < minLen || n > maxLen || !utf8.ValidString(v) {
		p.add(field, "invalid_length")
	}
}

func (p *problems) oneOf(field, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	p.add(field, "not_allowed")
}

func (p *problems) meta(field string, m map[string]any) {
	if len(m) > 50 {
		p.add(field, "too_many_properties")
	}
	for k := range m {
		if !metaKeyRE.MatchString(k) {
			p.add(field+"."+k, "invalid_key")
		}
	}
}

// query helpers --------------------------------------------------------------

func queryPage(r *http.Request, s *Server, scope string) (paging.Request, error) {
	var p problems
	pr := paging.Request{Limit: paging.DefaultLimit}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > paging.MaxLimit {
			p.add("limit", "out_of_range")
		} else {
			pr.Limit = n
		}
	}
	if err := p.err(); err != nil {
		return pr, err
	}
	after, err := s.cursors.Decode(scope, r.URL.Query().Get("cursor"))
	if err != nil {
		return pr, err
	}
	pr.After = after
	return pr, nil
}

// queryCutoff parses ?cutoff; explicit reports whether it was given.
func queryCutoff(r *http.Request, now time.Time) (cutoff time.Time, explicit bool, err error) {
	v := r.URL.Query().Get("cutoff")
	if v == "" {
		return now, false, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return now, false, apperr.New(apperr.ValidationFailed, "Invalid cutoff", apperr.Detail{Field: "cutoff", Reason: "invalid_timestamp"})
	}
	t = t.UTC()
	if t.After(now) {
		t = now
	}
	return t, true, nil
}

func parseDate(field, v string) (civil.Date, error) {
	d, err := civil.ParseDate(v)
	if err != nil || !d.IsValid() {
		return civil.Date{}, apperr.New(apperr.ValidationFailed, "Invalid date", apperr.Detail{Field: field, Reason: "invalid_date"})
	}
	return d, nil
}

func queryInt(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, apperr.New(apperr.ValidationFailed, "Invalid "+name, apperr.Detail{Field: name, Reason: "out_of_range"})
	}
	return n, nil
}

func parseStaleAfter(v string) (time.Duration, bool) {
	m := staleRE.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "d" {
		return time.Duration(n) * 24 * time.Hour, true
	}
	return time.Duration(n) * time.Hour, true
}

func formatStaleAfter(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(int(d/time.Hour)) + "h"
}
