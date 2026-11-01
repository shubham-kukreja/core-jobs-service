// Package notes stores analyst notes. A [[key]] in a note body becomes a
// reference, so notes can be listed per series, node, parameter or version.
//
// Bodies are stored as written; clients render them with a sanitising
// Markdown renderer. The API never returns rendered HTML.
package notes

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// Limits on note size.
const (
	MaxTitleLen = 200
	MaxBodyLen  = 100_000
)

// Ref is a reference parsed from a note body.
type Ref struct {
	Kind string // series | node | parameter | version | backtest
	Key  string
}

// Note is a stored note.
type Note struct {
	ID        uuid.UUID
	Title     string
	BodyMD    string
	AuthorID  int64
	Refs      []Ref
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Filter narrows List.
type Filter struct {
	Ref  *Ref
	Page paging.Request
}

// Store is notes CRUD. Update and Delete are allowed for the author or an admin;
// the caller enforces that.
type Store interface {
	Create(ctx context.Context, tx pgx.Tx, title, body string, authorID int64) (Note, error)
	Get(ctx context.Context, id uuid.UUID) (Note, error)
	List(ctx context.Context, f Filter) (paging.Page[Note], error)
	Update(ctx context.Context, tx pgx.Tx, id uuid.UUID, title, body string) (Note, error)
	Delete(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
	Search(ctx context.Context, q string, limit int) ([]Note, error)
}

var ErrNotFound = errors.New("notes: not found")
