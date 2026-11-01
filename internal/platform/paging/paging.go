// Package paging carries keyset pagination between the API and repositories.
// The API owns cursor encoding (signed, opaque to clients); repositories see
// only the decoded keyset position.
package paging

const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// Request asks for one page. After is the keyset position of the last item
// already seen (empty for the first page).
type Request struct {
	Limit int
	After string
}

// Normalize clamps Limit into [1, MaxLimit].
func (r Request) Normalize() Request {
	switch {
	case r.Limit <= 0:
		r.Limit = DefaultLimit
	case r.Limit > MaxLimit:
		r.Limit = MaxLimit
	}
	return r
}

// Page is one page of results. Next is the keyset position to pass as
// Request.After for the following page, or empty when there are no more.
type Page[T any] struct {
	Items []T
	Next  string
}
