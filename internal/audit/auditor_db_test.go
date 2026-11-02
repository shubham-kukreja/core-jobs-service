package audit_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/paging"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

func TestRecordAndQuery(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	a := audit.NewPG(db.Runtime)

	for i := range 3 {
		err := database.InTx(ctx, db.Runtime, func(tx pgx.Tx) error {
			return a.Record(ctx, tx, audit.Entry{
				ActorKind: audit.ActorSystem, RequestID: "req_test", IP: netip.MustParseAddr("10.0.0.1"),
				Entity: "series", EntityID: "pe.nifty100", Action: "update",
				Before: map[string]any{"n": i, "token": "secret-value"}, After: map[string]any{"n": i + 1},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := a.Query(ctx, audit.Filter{Entity: "series", Page: paging.Request{Limit: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == "" {
		t.Fatalf("first page: %d items, next %q", len(page.Items), page.Next)
	}
	if page.Items[0].ID < page.Items[1].ID {
		t.Fatal("want newest first")
	}
	if strings.Contains(string(page.Items[0].Before), "secret") {
		t.Fatalf("secret stored: %s", page.Items[0].Before)
	}
	next, err := a.Query(ctx, audit.Filter{Entity: "series", Page: paging.Request{Limit: 2, After: page.Next}})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.Next != "" {
		t.Fatalf("second page: %d items, next %q", len(next.Items), next.Next)
	}
}

func TestRecordRequiresTx(t *testing.T) {
	a := audit.NewPG(nil)
	if err := a.Record(context.Background(), nil, audit.Entry{ActorKind: audit.ActorSystem, Entity: "x", EntityID: "1", Action: "y"}); err == nil {
		t.Fatal("want error without tx")
	}
}
