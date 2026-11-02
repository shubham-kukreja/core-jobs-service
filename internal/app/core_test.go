package app

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
)

type fakeOutbox struct {
	marked []int64
}

func (f *fakeOutbox) Claim(context.Context, pgx.Tx, int) ([]datapoints.OutboxEvent, error) {
	return []datapoints.OutboxEvent{{ID: 7, Topic: datapoints.TopicWritten, Payload: []byte(`{"a":1}`)}}, nil
}

func (f *fakeOutbox) MarkDispatched(_ context.Context, _ pgx.Tx, ids []int64) error {
	f.marked = append(f.marked, ids...)
	return nil
}

func TestOutboxSourceMapsEvents(t *testing.T) {
	fo := &fakeOutbox{}
	src := outboxSource{fo}
	evs, err := src.Claim(context.Background(), nil, 10)
	if err != nil || len(evs) != 1 || evs[0].ID != 7 || evs[0].Topic != datapoints.TopicWritten || string(evs[0].Payload) != `{"a":1}` {
		t.Fatalf("got %+v, %v", evs, err)
	}
	if err := src.MarkDispatched(context.Background(), nil, []int64{7}); err != nil || len(fo.marked) != 1 {
		t.Fatalf("mark: %v %v", fo.marked, err)
	}
}

func TestDerivationsRejectedUntilPhase2(t *testing.T) {
	_, err := noDerivations{}.ValidateDerivation(catalog.Derivation{Fn: "expr"})
	if !apperr.Is(err, apperr.NotAvailable) {
		t.Fatalf("want not_available, got %v", err)
	}
}
