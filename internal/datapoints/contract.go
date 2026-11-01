// Package datapoints owns every stored value. There is one write path
// (Writer: validate → store → outbox event, in the caller's transaction) and
// one point-in-time read path (Reader).
package datapoints

import (
	"context"
	"errors"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Value is exactly one of a number, a boolean or a category, or null.
type Value struct {
	Num  *float64
	Bool *bool
	Cat  *string
}

// IsNull reports whether no value is set.
func (v Value) IsNull() bool { return v.Num == nil && v.Bool == nil && v.Cat == nil }

// Num returns a numeric value.
func Num(f float64) Value { return Value{Num: &f} }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{Bool: &b} }

// Cat returns a categorical value.
func Cat(s string) Value { return Value{Cat: &s} }

// Months is a look-back window length.
type Months int

// Point is one value to write.
type Point struct {
	SeriesKey string
	ObsDate   civil.Date
	Value     Value
	// KnownAt is when the value became knowable. Zero means now. Computed
	// rows pass max(inputs.KnownAt); historical imports pass an estimate from
	// the source's publication lag.
	KnownAt  time.Time
	Note     string
	Inputs   []int64 // computed rows only: lineage
	DerivVer int     // computed rows only
}

// Origin distinguishes cascade output from primary writes so the cascade
// never re-triggers itself.
type Origin string

const (
	OriginPrimary Origin = "primary"
	OriginCascade Origin = "cascade"
)

// WriteMeta says who is writing and why.
type WriteMeta struct {
	Source   string // source key: 'analyst', 'override', 'computed', 'file_import', connector key
	ActorID  *int64 // user for analyst/override rows
	JobRunID *int64
	Origin   Origin
}

// Rejection is one point that failed validation. Rejections never fail the
// whole write.
type Rejection struct {
	Index     int
	SeriesKey string
	Reason    string // unknown_series | wrong_type | out_of_range | not_allowed | inactive_series | not_writable
	Message   string
}

// WriteResult summarises a write.
type WriteResult struct {
	BatchID  uuid.UUID
	Written  int
	Skipped  int // identical to the current resolved value, or same input_hash
	Rejected []Rejection
	// IDs of written rows, in input order (0 for skipped or rejected points).
	IDs []int64
}

// Writer is the only way values enter the system.
type Writer interface {
	// Write validates pts against their series, skips no-ops, inserts rows and
	// one outbox event (topic TopicWritten), all in tx.
	Write(ctx context.Context, tx pgx.Tx, meta WriteMeta, pts []Point) (WriteResult, error)
	// Retract hides datapoints from reads with cutoff after now. The rows stay
	// in history; a retraction row records who and why.
	Retract(ctx context.Context, tx pgx.Tx, meta WriteMeta, ids []int64, reason string) error
}

// ReadOpts controls point-in-time resolution.
type ReadOpts struct {
	Cutoff  time.Time // knowledge cutoff; zero = now
	Sources []string  // restrict to these source keys; default = series.source_priority
}

// Resolved is the value a reader sees for one (series, obs_date) at a cutoff.
type Resolved struct {
	ID        int64
	SeriesKey string
	ObsDate   civil.Date
	Value     Value
	Source    string
	KnownAt   time.Time
	DerivVer  int
	Inputs    []int64
}

// Datapoint is a stored row, including retracted and superseded versions.
type Datapoint struct {
	Resolved
	Status       string // active | retracted
	SupersedesID *int64
	ActorID      *int64
	JobRunID     *int64
	BatchID      uuid.UUID
	Note         string
	CreatedAt    time.Time
}

// Lineage is a datapoint and, recursively, the datapoints it was computed from.
type Lineage struct {
	Datapoint Resolved
	Inputs    []Lineage
}

// Reader resolves values point-in-time: for each (series, obs_date) it keeps
// active rows with known_at <= cutoff, takes the latest per source, then
// picks the first source in the series' source_priority.
type Reader interface {
	At(ctx context.Context, series string, d civil.Date, o ReadOpts) (*Resolved, error)
	Range(ctx context.Context, series string, from, to civil.Date, o ReadOpts) ([]Resolved, error)
	// History returns resolved values with obs_date in (upTo - window, upTo].
	History(ctx context.Context, series string, upTo civil.Date, window Months, o ReadOpts) ([]Resolved, error)
	// Snapshot resolves many series at one date. Missing series are omitted.
	Snapshot(ctx context.Context, series []string, d civil.Date, o ReadOpts) (map[string]Resolved, error)
	// Versions returns every row for (series, d): all sources, all revisions.
	Versions(ctx context.Context, series string, d civil.Date) ([]Datapoint, error)
	Get(ctx context.Context, id int64) (Datapoint, error)
	Lineage(ctx context.Context, id int64, maxDepth int) (Lineage, error)
	// Batch returns what one write batch touched, for outbox consumers.
	Batch(ctx context.Context, batchID uuid.UUID) (BatchSummary, error)
}

// BatchSummary describes one write batch.
type BatchSummary struct {
	BatchID   uuid.UUID
	SeriesIDs []int64
	MinDate   civil.Date
	MaxDate   civil.Date
	Origin    Origin
}

// Outbox topics.
const (
	TopicWritten = "datapoints.written"
)

// WrittenEvent is the payload of TopicWritten.
type WrittenEvent struct {
	BatchID   uuid.UUID  `json:"batch_id"`
	SeriesIDs []int64    `json:"series_ids"`
	Layers    []string   `json:"layers"`
	MinDate   civil.Date `json:"min_date"`
	MaxDate   civil.Date `json:"max_date"`
	Origin    Origin     `json:"origin"`
}

// OutboxEvent is one undispatched outbox row.
type OutboxEvent struct {
	ID        int64
	Topic     string
	Payload   []byte
	CreatedAt time.Time
}

// Outbox is consumed by the jobs dispatcher.
type Outbox interface {
	// Claim locks up to limit undispatched rows (FOR UPDATE SKIP LOCKED) in tx.
	Claim(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxEvent, error)
	MarkDispatched(ctx context.Context, tx pgx.Tx, ids []int64) error
}

// RawPayload indexes a stored connector response.
type RawPayload struct {
	ID          int64
	SourceKey   string
	IngestRunID *int64
	FetchedAt   time.Time
	Request     map[string]any // credentials stripped by the connector
	BlobURI     string
	SHA256      []byte
	Bytes       int64
}

// RawPayloads records raw responses for audit and replay.
type RawPayloads interface {
	Record(ctx context.Context, tx pgx.Tx, p RawPayload) (int64, error)
}

var (
	ErrNotFound = errors.New("datapoints: not found")
	// ErrAlreadyRetracted is returned when retracting a row twice.
	ErrAlreadyRetracted = errors.New("datapoints: already retracted")
)
