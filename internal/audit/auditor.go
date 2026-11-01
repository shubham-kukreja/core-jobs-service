package audit

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit/internal/db"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
)

// PG is the Postgres-backed Auditor.
type PG struct {
	q *db.Queries
}

var _ Auditor = (*PG)(nil)

// NewPG returns an Auditor that reads through conn (a pool) and writes in the
// caller's transaction.
func NewPG(conn database.DBTX) *PG { return &PG{q: db.New(conn)} }

// Record writes e in tx after redacting its snapshots.
func (a *PG) Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	if tx == nil {
		return errors.New("audit: Record requires a transaction")
	}
	if e.Entity == "" || e.EntityID == "" || e.Action == "" {
		return errors.New("audit: entity, entity id and action are required")
	}
	switch e.ActorKind {
	case ActorUser, ActorAPIKey, ActorSystem:
	default:
		return fmt.Errorf("audit: unknown actor kind %q", e.ActorKind)
	}
	before, err := Snapshot(e.Before)
	if err != nil {
		return err
	}
	after, err := Snapshot(e.After)
	if err != nil {
		return err
	}
	p := db.InsertEntryParams{
		ActorID:   e.ActorID,
		ActorKind: string(e.ActorKind),
		Entity:    e.Entity,
		EntityID:  e.EntityID,
		Action:    e.Action,
		Before:    before,
		After:     after,
	}
	if e.RequestID != "" {
		p.RequestID = &e.RequestID
	}
	if e.IP.IsValid() {
		ip := e.IP.Unmap()
		p.Ip = &ip
	}
	if err := a.q.WithTx(tx).InsertEntry(ctx, p); err != nil {
		return fmt.Errorf("audit: record: %w", err)
	}
	return nil
}

// Query returns entries newest first. Page.Next is the id of the last entry
// returned when more remain.
func (a *PG) Query(ctx context.Context, f Filter) (paging.Page[Record], error) {
	pr := f.Page.Normalize()
	p := db.QueryEntriesParams{ActorID: f.ActorID, Since: f.Since, Lim: int32(pr.Limit + 1)} //nolint:gosec // Normalize bounds Limit to MaxLimit.
	if f.Entity != "" {
		p.Entity = &f.Entity
	}
	if f.EntityID != "" {
		p.EntityID = &f.EntityID
	}
	if pr.After != "" {
		id, err := strconv.ParseInt(pr.After, 10, 64)
		if err != nil || id <= 0 {
			return paging.Page[Record]{}, errors.New("audit: invalid page position")
		}
		p.BeforeID = &id
	}
	rows, err := a.q.QueryEntries(ctx, p)
	if err != nil {
		return paging.Page[Record]{}, fmt.Errorf("audit: query: %w", err)
	}
	var page paging.Page[Record]
	for i, r := range rows {
		if i == pr.Limit {
			page.Next = strconv.FormatInt(page.Items[i-1].ID, 10)
			break
		}
		rec := Record{
			ID: r.ID, At: r.At, ActorID: r.ActorID, ActorKind: ActorKind(r.ActorKind),
			Entity: r.Entity, EntityID: r.EntityID, Action: r.Action, Before: r.Before, After: r.After,
		}
		if r.RequestID != nil {
			rec.RequestID = *r.RequestID
		}
		page.Items = append(page.Items, rec)
	}
	return page, nil
}
