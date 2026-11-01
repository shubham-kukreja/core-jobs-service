// Package audit records who changed what. Every API mutation writes one
// entry in the same transaction as the change; the table is append-only.
package audit

import (
	"context"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// ActorKind is who acted.
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorAPIKey ActorKind = "api_key"
	ActorSystem ActorKind = "system"
)

// Entry is one audited action. Before and After are marshalled to JSON after
// redaction: fields tagged `audit:"-"` and keys that look sensitive
// (password, token, secret, hash) are removed.
type Entry struct {
	ActorID   *int64
	ActorKind ActorKind
	RequestID string
	IP        netip.Addr
	Entity    string // 'series', 'model_config', 'datapoint', 'session', …
	EntityID  string
	Action    string // create | update | retract | approve | publish | login | logout | …
	Before    any
	After     any
}

// Record is a stored entry.
type Record struct {
	ID        int64
	At        time.Time
	ActorID   *int64
	ActorKind ActorKind
	RequestID string
	Entity    string
	EntityID  string
	Action    string
	Before    []byte
	After     []byte
}

// Filter narrows Query.
type Filter struct {
	Entity   string
	EntityID string
	ActorID  *int64
	Since    *time.Time
	Page     paging.Request
}

// Auditor writes and reads the audit log.
type Auditor interface {
	Record(ctx context.Context, tx pgx.Tx, e Entry) error
	Query(ctx context.Context, f Filter) (paging.Page[Record], error)
}
