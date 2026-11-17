package ingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/blob"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

// memBlobs is an in-memory blob.Store.
type memBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *memBlobs) Put(_ context.Context, key, ct string, r io.Reader, max int64) (blob.Object, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return blob.Object{}, err
	}
	if int64(len(data)) > max {
		return blob.Object{}, blob.ErrTooLarge
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	uri := "blob://test/" + key
	b.m[uri] = data
	sum := sha256.Sum256(data)
	return blob.Object{URI: uri, Key: key, Size: int64(len(data)), SHA256: sum[:], ContentType: ct}, nil
}

func (b *memBlobs) Get(_ context.Context, uri string) (io.ReadCloser, blob.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.m[uri]
	if !ok {
		return nil, blob.Object{}, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), blob.Object{URI: uri}, nil
}

func (b *memBlobs) Delete(_ context.Context, uri string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, uri)
	return nil
}

const csvFile = "Month,10Y G-sec Yield %,AAA Spread (bps)\nJan-2026,6.70,75\nFeb-2026,6.65,70\nMar-2026,99,70\n"

func TestImportFileEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	clk := clock.NewFake(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	for _, sql := range []string{
		`INSERT INTO auth.users(email,name,role) VALUES ('riya@example.com','Riya','analyst')`,
		`INSERT INTO catalog.sources(key,kind,name) VALUES ('file_import','file_import','File')`,
	} {
		if _, err := db.Owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var uid int64
	if err := db.Owner.QueryRow(ctx, `SELECT id FROM auth.users`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	cat := catalog.New(db.Runtime, anyFormula{})
	lo, hi := 0.0, 30.0
	for _, key := range []string{"yield.gsec10y", "pct.gsec"} {
		layer := catalog.LayerRaw
		if key == "pct.gsec" {
			layer = catalog.LayerJudgment
		}
		if err := database.InTx(ctx, db.Runtime, func(tx pgx.Tx) error {
			_, err := cat.SaveSeries(ctx, tx, catalog.SeriesInput{Key: key, Name: key, Layer: layer, ValueType: catalog.ValueNum,
				Frequency: catalog.FreqMonthly, Validation: catalog.Validation{Min: &lo, Max: &hi}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	dp := datapoints.New(db.Runtime, cat, clk)
	ups := ingest.NewUploads(db.Runtime, &memBlobs{m: map[string][]byte{}}, clk, 1<<20)
	maps := ingest.NewMappings(db.Runtime, cat)

	m := ingest.Mapping{HeaderRowMatch: "Month", DateColumn: ingest.DateColumn{Name: "Month", Format: "Jan-2006", ObsDate: "month_end"},
		Columns: []ingest.ColumnMap{{Name: "10Y G-sec Yield %", Series: "yield.gsec10y"}}, IgnoreColumns: []string{"AAA Spread (bps)"},
		KnownAt: "obs_date_plus_days:5"}
	bad := m
	bad.Columns = []ingest.ColumnMap{{Name: "x", Series: "pct.gsec"}}
	if _, err := maps.Save(ctx, "rates", "Rates", bad, uid); !apperr.Is(err, apperr.ValidationFailed) {
		t.Fatalf("non-raw series accepted: %v", err)
	}
	saved, err := maps.Save(ctx, "rates", "Rates", m, uid)
	if err != nil || saved.Version != 1 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	if again, err := maps.Save(ctx, "rates", "Rates", m, uid); err != nil || again.Version != 1 {
		t.Fatalf("unchanged save bumped version: %+v %v", again, err)
	}
	if list, err := maps.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	if _, err := ups.Store(ctx, ingest.UploadInput{Filename: "x.pdf", Body: strings.NewReader("%PDF-1.4\x00\x01"), UploadedBy: uid}); !apperr.Is(err, apperr.ValidationFailed) {
		t.Fatalf("pdf accepted: %v", err)
	}
	up, err := ups.Store(ctx, ingest.UploadInput{Filename: "../rates.csv", Body: strings.NewReader(csvFile), UploadedBy: uid})
	if err != nil || up.Filename != "rates.csv" || up.ContentType != ingest.MediaCSV {
		t.Fatalf("upload: %+v %v", up, err)
	}

	im := &ingest.Importer{DB: db.Runtime, Uploads: ups, Mappings: maps, Parser: ingest.NewParser(), Writer: dp, Clock: clk, Limits: ingest.DefaultLimits}
	count := func() int {
		var n int
		if err := db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM dp.datapoints) + (SELECT count(*) FROM dp.outbox)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	dry, err := im.Import(ctx, ingest.ImportArgs{UploadID: up.UUID, MappingID: "rates", DryRun: true}, 0)
	if err != nil || dry.RowsWritten != 2 || len(dry.Rejected) != 1 || len(dry.Preview) != 3 || count() != 0 {
		t.Fatalf("dry run: %+v %v (rows %d)", dry, err, count())
	}
	res, err := im.Import(ctx, ingest.ImportArgs{UploadID: up.UUID, MappingID: "rates"}, 0)
	if err != nil || res.RowsWritten != 2 || len(res.Rejected) != 1 || res.Rejected[0].Reason != datapoints.ReasonOutOfRange {
		t.Fatalf("import: %+v %v", res, err)
	}
	r, err := dp.At(ctx, "yield.gsec10y", civil.Date{Year: 2026, Month: 1, Day: 31}, datapoints.ReadOpts{})
	if err != nil || r == nil || *r.Value.Num != 6.70 || r.Source != "file_import" {
		t.Fatalf("read back: %+v %v", r, err)
	}
	re, err := im.Import(ctx, ingest.ImportArgs{UploadID: up.UUID, MappingID: "rates"}, 0)
	if err != nil || re.RowsWritten != 0 || re.RowsSkipped != 2 {
		t.Fatalf("re-import: %+v %v", re, err)
	}

	_, err = im.Import(ctx, ingest.ImportArgs{UploadID: up.UUID, MappingID: "missing"}, 0)
	if !jobs.IsPermanent(err) {
		t.Fatalf("missing mapping must fail permanently: %v", err)
	}
}

type anyFormula struct{}

func (anyFormula) ValidateDerivation(d catalog.Derivation) ([]string, error) { return nil, nil }

type fakeConnector struct{}

func (fakeConnector) Key() string      { return "fake" }
func (fakeConnector) Series() []string { return []string{"yield.gsec10y"} }
func (fakeConnector) Fetch(_ context.Context, req ingest.FetchRequest) (ingest.FetchResult, error) {
	if req.From.After(req.To) {
		return ingest.FetchResult{}, errors.New("bad range")
	}
	return ingest.FetchResult{Points: []datapoints.Point{{SeriesKey: "yield.gsec10y", ObsDate: req.To, Value: datapoints.Num(6.7)}}}, nil
}

// TestConnectorContract is the contract every Phase 5 connector must pass.
func TestConnectorContract(t *testing.T) {
	reg := ingest.NewConnectorRegistry()
	reg.Register(fakeConnector{})
	c, ok := reg.Get("fake")
	if !ok || len(reg.Keys()) != 1 {
		t.Fatal("connector not registered")
	}
	d := civil.Date{Year: 2026, Month: 1, Day: 31}
	res, err := c.Fetch(context.Background(), ingest.FetchRequest{SeriesKeys: c.Series(), From: d, To: d})
	if err != nil {
		t.Fatal(err)
	}
	series := map[string]bool{}
	for _, s := range c.Series() {
		series[s] = true
	}
	for _, p := range res.Points {
		if !series[p.SeriesKey] || p.ObsDate.Before(d) || p.ObsDate.After(d) {
			t.Fatalf("point outside request: %+v", p)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate connector did not panic")
		}
	}()
	reg.Register(fakeConnector{})
}
