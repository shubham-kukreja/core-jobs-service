package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/app"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/derive"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/blob"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
	"github.com/shubham-kukreja/core-jobs-service/internal/seed"
)

// Gate G2: import the sheet's logs → every percentile and derived series
// computes → no value uses future data → re-import is a no-op.
//
// The fixture is the workbook's Data Trackers sheet laid out exactly as in
// the workbook (title rows, four "Month" headers, formula columns left blank),
// ending with the workbook's real Jan–Mar 2026 rows. The workbook holds only
// those three months, and percentiles need 24 observations, so 33 months of
// deterministic history come first. The upload template's new columns, the
// analyst's breadth counts and judgments fill the rest of the inputs.

const g2Months = 36 // Apr-2023 … Mar-2026

var g2Last = civil.Date{Year: 2026, Month: 3, Day: 31}

func g2Month(i int) civil.Date { return calendar.MonthEnd(calendar.AddMonths(g2Last, i-(g2Months-1))) }

func monthLabel(d civil.Date) string { return d.In(time.UTC).Format("Jan-2006") }

// wave is a deterministic, non-monotonic series around base.
func wave(base, amp, period, phase float64, i int) float64 {
	return base * (1 + amp*math.Sin(2*math.Pi*float64(i)/period+phase))
}

// trackerRows are the workbook's logged rows (Jan, Feb, Mar 2026), verbatim.
var trackerRows = map[string][3][]float64{
	"valuation": {{22.5, 33.1, 30.2}, {21.8, 31.9, 28.8}, {22.1, 32.4, 29.5}},
	"rates":     {{6.75, 6.0, 7.4}, {6.68, 6.0, 7.3}, {6.72, 5.75, 7.38}},
	"metals":    {{78500, 92000}, {80200, 95500}, {81000, 97800}},
	"flows":     {{1500, 2200, 3100, 4500, 600}, {1300, 2500, 3600, 5200, 550}, {1600, 2300, 3400, 4800, 700}},
}

func trackerValue(log string, col, i int) float64 {
	if j := i - (g2Months - 3); j >= 0 {
		return trackerRows[log][j][col]
	}
	switch log {
	case "valuation":
		return []float64{wave(21.5, 0.08, 17, 0, i), wave(30, 0.10, 13, 1, i), wave(27, 0.12, 11, 2, i)}[col]
	case "rates":
		g := wave(7.0, 0.06, 19, 0.5, i)
		return []float64{g, 6.5 - 0.25*float64(i/12), g + 0.55 + 0.15*math.Sin(float64(i)/3)}[col]
	case "metals":
		return []float64{55000 * math.Pow(1.011, float64(i)) * (1 + 0.03*math.Sin(float64(i)/2)),
			70000 * math.Pow(1.008, float64(i)) * (1 + 0.05*math.Sin(float64(i)/3+1))}[col]
	default: // flows
		return []float64{wave(1500, 0.4, 7, 0, i), wave(2200, 0.3, 9, 1, i), wave(3000, 0.35, 8, 2, i),
			wave(4200, 0.3, 10, 3, i), wave(550, 0.5, 6, 4, i)}[col]
	}
}

// dataTrackersGrid lays the four logs out as the workbook does.
func dataTrackersGrid() [][]string {
	logs := []struct {
		key, title string
		header     []string
		cols       int
	}{
		{"valuation", "Equity valuation log (index P/E, niftyindices.com)", []string{"Month", "Nifty 100 P/E", "Midcap 150 P/E", "Smallcap 250 P/E", "Mid–Large Spread", "Small–Large Spread", "Mid Spread %ile", "Small Spread %ile"}, 3},
		{"rates", "Rates & credit log (RBI/CCIL, FIMMDA or broker reports)", []string{"Month", "10Y G-sec Yield %", "Repo Rate %", "3Y AAA Corp Yield %", "AAA Spread (bps)", "10Y Yield %ile", "Spread %ile"}, 3},
		{"metals", "Precious metals log", []string{"Month", "Gold ₹/10g", "Silver ₹/kg", "Gold/Silver Ratio", "Gold 12M Return %", "Ratio %ile", "Gold Ret %ile"}, 2},
		{"flows", "Category flow log (AMFI monthly net flows, ₹ crore)", []string{"Month", "Large Cap", "Mid Cap", "Small Cap", "Sectoral/Thematic", "Gold ETF", "Small Flow %ile", "Sectoral Flow %ile"}, 5},
	}
	grid := [][]string{{"Data Trackers"}, {"Log monthly. Percentiles build automatically."}, {}}
	for _, l := range logs {
		grid = append(grid, []string{l.title}, l.header)
		for i := range 48 { // the workbook's 48-row capacity per log
			row := make([]string, len(l.header))
			if i < g2Months {
				row[0] = monthLabel(g2Month(i))
				for c := range l.cols {
					row[1+c] = strconv.FormatFloat(trackerValue(l.key, c, i), 'f', 2, 64)
				}
			}
			grid = append(grid, row)
		}
		grid = append(grid, []string{})
	}
	return grid
}

// extrasValue fills the upload template's new columns with plausible,
// deterministic values within each series' validation bounds.
func extrasValue(key string, i int) float64 {
	h := sha256.Sum256([]byte(key))
	phase := float64(h[0]) / 40
	period := 7 + float64(h[1]%9)
	x := float64(i)
	osc := func(base, amp float64) float64 { return base + amp*math.Sin(2*math.Pi*x/period+phase) }
	grow := func(base, rate float64) float64 {
		return base * math.Pow(1+rate, x) * (1 + 0.04*math.Sin(2*math.Pi*x/period+phase))
	}
	switch {
	case key == "val.sector.banking":
		return osc(3, 0.6)
	case strings.HasPrefix(key, "pe."), strings.HasPrefix(key, "val.sector."):
		return osc(21, 4)
	case key == "tri.nifty50":
		return grow(30000, 0.011)
	case strings.HasPrefix(key, "px."):
		return grow(20000, 0.004+float64(h[2]%10)/1000)
	case strings.HasPrefix(key, "eps.fy1."):
		return grow(1000, 0.008)
	case strings.HasPrefix(key, "epsg.fy1."):
		return osc(12+float64(h[3]%8), 5)
	case strings.HasPrefix(key, "margin.ebitda."):
		return osc(16, 3)
	case key == "cpi.yoy":
		return osc(5, 1.2)
	case key == "cpi.index":
		return 180 * math.Pow(1.004, x)
	case key == "tips.10y":
		return osc(1.8, 0.6)
	case key == "pmi.global":
		return osc(51, 2.5)
	case strings.HasPrefix(key, "flows."):
		return osc(4000, 3500)
	case strings.HasPrefix(key, "aum."):
		return grow(200000, 0.01)
	case key == "bondidx.ret12m":
		return osc(7, 1.5)
	default: // yields and YTMs
		return osc(7, 0.4)
	}
}

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
	uri := "blob://g2/" + key
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

type g2 struct {
	db   *testdb.DB
	core *app.Core
	uid  int64
	docs seed.Documents
}

func setupG2(ctx context.Context, t *testing.T) *g2 {
	t.Helper()
	return newG2(ctx, t, clock.System{}, nil)
}

// newG2 seeds the shipped configs (after tweak, if any) on a fresh database
// and starts a worker, all on clk.
func newG2(ctx context.Context, t *testing.T, clk clock.Clock, tweak func(seed.Documents)) *g2 {
	t.Helper()
	db := testdb.New(t)
	var uid int64
	if err := db.Owner.QueryRow(ctx,
		`INSERT INTO auth.users(email,name,role) VALUES ('lead@example.com','Lead','admin') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	core, err := app.NewCore(db.Runtime, testLogger(), clk)
	if err != nil {
		t.Fatal(err)
	}
	core.EnableIngest(&memBlobs{m: map[string][]byte{}}, 10<<20)
	docs, err := seed.Validate(filepath.Join("..", "configs", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	if tweak != nil {
		tweak(docs)
	}
	if _, err := seed.Apply(ctx, core.SeedTarget(), docs, uid); err != nil {
		t.Fatal(err)
	}
	w, err := core.NewWorker()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Stop(context.Background()) })
	g := &g2{db: db, core: core, uid: uid, docs: docs}
	g.drain(ctx, t) // the seed's rebuild over empty history
	return g
}

// testLogger discards logs unless E2E_LOG is set, for debugging failures.
func testLogger() *slog.Logger {
	if os.Getenv("E2E_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drain waits until no job is queued or running and the outbox is empty,
// twice in a row, and fails on any failed run.
func (g *g2) drain(ctx context.Context, t *testing.T) {
	t.Helper()
	quiet := 0
	for quiet < 2 {
		var busy, pending int
		if err := g.db.Owner.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM jobs.job_runs WHERE status IN ('queued','running')),
  (SELECT count(*) FROM dp.outbox WHERE dispatched_at IS NULL)`).Scan(&busy, &pending); err != nil {
			t.Fatal(err)
		}
		if busy == 0 && pending == 0 {
			quiet++
		} else {
			quiet = 0
		}
		select {
		case <-ctx.Done():
			t.Fatalf("jobs did not drain: %d running, %d outbox events pending", busy, pending)
		case <-time.After(300 * time.Millisecond):
		}
	}
	rows, err := g.db.Owner.Query(ctx, `SELECT kind, coalesce(error::text, '') FROM jobs.job_runs WHERE status = 'failed'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, msg string
		if err := rows.Scan(&kind, &msg); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("%s failed: %s", kind, msg)
	}
}

// importFile uploads data and runs import.file with each mapping in turn,
// returning rows written per mapping.
func (g *g2) importFile(ctx context.Context, t *testing.T, name string, data []byte, mappings ...string) map[string]int {
	t.Helper()
	up, err := g.core.Uploads.Store(ctx, ingest.UploadInput{Filename: name, Body: bytes.NewReader(data), UploadedBy: g.uid})
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]int{}
	for _, m := range mappings {
		ref, err := g.core.Jobs.Enqueue(ctx, ingest.ImportArgs{UploadID: up.UUID, MappingID: m},
			jobs.WithTrigger(jobs.TriggerUser), jobs.WithUser(g.uid))
		if err != nil {
			t.Fatal(err)
		}
		g.drain(ctx, t)
		var result []byte
		if err := g.db.Owner.QueryRow(ctx, `SELECT result FROM jobs.job_runs WHERE uuid = $1`, ref.UUID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		var res struct {
			RowsWritten int   `json:"rows_written"`
			Rejected    []any `json:"rejected"`
		}
		if err := json.Unmarshal(result, &res); err != nil {
			t.Fatalf("%s result %s: %v", m, result, err)
		}
		if len(res.Rejected) > 0 {
			t.Fatalf("%s rejected rows: %v", m, res.Rejected)
		}
		written[m] = res.RowsWritten
	}
	return written
}

func (g *g2) mapping(t *testing.T, path string) ingest.Mapping {
	t.Helper()
	raw, err := json.Marshal(g.docs[filepath.Join("import_mappings", path)])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Mapping ingest.Mapping `json:"mapping"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Mapping
}

func extrasGrid(m ingest.Mapping) [][]string {
	header := []string{"Month"}
	for _, c := range m.Columns {
		header = append(header, c.Name)
	}
	grid := [][]string{header}
	for i := range g2Months {
		row := []string{monthLabel(g2Month(i))}
		for _, c := range m.Columns {
			row = append(row, strconv.FormatFloat(extrasValue(c.Series, i), 'f', 4, 64))
		}
		grid = append(grid, row)
	}
	return grid
}

// writeAnalyst enters breadth counts and every judgment for every month, as
// the analyst would through POST /v1/datapoints.
func (g *g2) writeAnalyst(ctx context.Context, t *testing.T, series []catalog.SeriesInput) {
	t.Helper()
	var pts []datapoints.Point
	for i := range g2Months {
		d := g2Month(i)
		known := d.In(time.UTC).AddDate(0, 0, 5)
		for _, s := range series {
			var v datapoints.Value
			switch {
			case s.Layer == catalog.LayerJudgment:
				v = datapoints.Cat(s.AllowedValues[(i/3+len(s.Key))%len(s.AllowedValues)])
			case strings.HasPrefix(s.Key, "breadth.") && s.Layer == catalog.LayerRaw:
				v = datapoints.Num(math.Round(wave(40, 0.35, 9, float64(len(s.Key)), i)))
			default:
				continue
			}
			pts = append(pts, datapoints.Point{SeriesKey: s.Key, ObsDate: d, Value: v, KnownAt: known})
		}
	}
	if err := database.InTx(ctx, g.db.Runtime, func(tx pgx.Tx) error {
		res, err := g.core.Datapoints.Write(ctx, tx, datapoints.WriteMeta{Source: "analyst", ActorID: &g.uid}, pts)
		if err == nil && len(res.Rejected) > 0 {
			err = fmt.Errorf("rejected: %v", res.Rejected)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g.drain(ctx, t)
}

func (g *g2) count(ctx context.Context, t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := g.db.Owner.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestG2ImportComputeNoLookAheadReimport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	g := setupG2(ctx, t)
	series, err := seed.SeriesInputs(g.docs)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Import the sheet's four logs from one workbook, the extras, and the
	// analyst's entries.
	trackers := xlsx(t, "Data Trackers", dataTrackersGrid())
	logs := []string{"data_trackers.valuation", "data_trackers.rates", "data_trackers.metals", "data_trackers.flows"}
	written := g.importFile(ctx, t, "MultiAsset_Allocation_Framework_v2.xlsx", trackers, logs...)
	wantRows := map[string]int{"data_trackers.valuation": 3, "data_trackers.rates": 3, "data_trackers.metals": 2, "data_trackers.flows": 5}
	for m, cols := range wantRows {
		if written[m] != cols*g2Months {
			t.Errorf("%s wrote %d rows, want %d", m, written[m], cols*g2Months)
		}
	}
	extrasMap := g.mapping(t, "extras_log.yaml")
	extras := xlsx(t, "Extras", extrasGrid(extrasMap))
	if w := g.importFile(ctx, t, "extras.xlsx", extras, "upload.extras"); w["upload.extras"] != len(extrasMap.Columns)*g2Months {
		t.Errorf("extras wrote %d rows, want %d", w["upload.extras"], len(extrasMap.Columns)*g2Months)
	}
	g.writeAnalyst(ctx, t, series)

	// 2. Every derived, window, percentile, score and computed trigger series
	// has a value at the last month.
	var missing []string
	for _, s := range series {
		if s.Derivation == nil {
			continue
		}
		r, err := g.core.Datapoints.At(ctx, s.Key, g2Last, datapoints.ReadOpts{})
		if err != nil || r == nil || r.Value.IsNull() {
			missing = append(missing, s.Key)
		}
	}
	sort.Strings(missing)
	checked := 0
	for _, s := range series {
		if s.Derivation != nil {
			checked++
		}
	}
	t.Logf("computed series with a value at %s: %d of %d", g2Last, checked-len(missing), checked)
	if len(missing) > 0 {
		t.Errorf("%d computed series have no value at %s: %v", len(missing), g2Last, missing)
	}

	// Spot checks against the workbook's own formulas on its Mar-2026 row.
	num := func(key string, d civil.Date) float64 {
		t.Helper()
		r, err := g.core.Datapoints.At(ctx, key, d, datapoints.ReadOpts{})
		if err != nil || r == nil || r.Value.Num == nil {
			t.Fatalf("%s @%s: %v %v", key, d, r, err)
		}
		return *r.Value.Num
	}
	for key, want := range map[string]float64{
		"spread.mid_large":   32.4 - 22.1,
		"spread.small_large": 29.5 - 22.1,
		"spread.aaa_bps":     (7.38 - 6.72) * 100,
		"ratio.gold_silver":  81000.0 * 100 / 97800,
	} {
		if got := num(key, g2Last); math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{"debt.liquid::liquidity", "debt.gilt::liquidity", "equity.large::eps_growth_diff"} {
		want := map[string]float64{"debt.liquid::liquidity": 90, "debt.gilt::liquidity": 75, "equity.large::eps_growth_diff": 50}[key]
		if got := num(key, g2Last); got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}

	// The sheet's first issue: Jan-2026's 10Y %ile ranked against Feb–Mar.
	// Here it ranks only against history up to Jan (PERCENTRANK.INC).
	jan := civil.Date{Year: 2026, Month: 1, Day: 31}
	var hist []float64
	for i := range g2Months {
		if !g2Month(i).After(jan) {
			hist = append(hist, math.Round(trackerValue("rates", 0, i)*100)/100)
		}
	}
	cur, less := hist[len(hist)-1], 0
	for _, v := range hist {
		if v < cur {
			less++
		}
	}
	if got, want := num("pctile.180m.yield.gsec10y", jan), 100*float64(less)/float64(len(hist)-1); math.Abs(got-want) > 1e-9 {
		t.Errorf("Jan-2026 10Y %%ile = %v, want %v from %d observations up to Jan", got, want, len(hist))
	}

	// The explain breakdown (P2-T3-07) reproduces every stored score through
	// its whole formula tree: no level's recomputation drifts from the
	// stored value. Equity - Valuation has the plan's shape.
	ex, err := g.core.Explainer.Explain(ctx, "l1.equity::valuation", civil.Date{}, datapoints.ReadOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.ObsDate != g2Last || len(ex.Parts) != 2 || ex.Parts[0].Alias != "gap" || ex.Parts[1].Alias != "pe" ||
		len(ex.Steps) == 0 || ex.Steps[len(ex.Steps)-1].Result == nil ||
		math.Abs(*ex.Steps[len(ex.Steps)-1].Result-num("l1.equity::valuation", g2Last)) > 1e-9 {
		t.Errorf("valuation explain = %s %d parts, steps %+v", ex.ObsDate, len(ex.Parts), ex.Steps)
	}
	scores, err := g.db.Owner.Query(ctx, `SELECT key FROM catalog.series WHERE layer = 'score' ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	var scoreKeys []string
	for scores.Next() {
		var k string
		if err := scores.Scan(&k); err != nil {
			t.Fatal(err)
		}
		scoreKeys = append(scoreKeys, k)
	}
	scores.Close()
	for _, k := range scoreKeys {
		ex, err := g.core.Explainer.Explain(ctx, k, g2Last, datapoints.ReadOpts{})
		if err != nil {
			t.Fatalf("explain %s: %v", k, err)
		}
		var walk func(e derive.Explanation)
		walk = func(e derive.Explanation) {
			for _, n := range e.Notes {
				t.Errorf("explain %s: %s: %s", k, e.Series.Key, n)
			}
			for _, p := range e.Parts {
				walk(p)
			}
		}
		walk(ex)
	}

	// 3. No computed value uses future data.
	// Trigger writes enqueue veto.evaluate (Phase 3): its veto.* rows carry
	// lineage to the triggers and are part of the look-ahead check below.
	if n := g.count(ctx, t, `SELECT count(*) FROM dp.datapoints d JOIN catalog.series s ON s.id = d.series_id WHERE s.layer = 'veto'`); n == 0 {
		t.Error("no veto rows written from the imported triggers")
	}

	n := assertNoLookAhead(ctx, t, g.db.Owner)
	t.Logf("look-ahead check covered %d computed rows", n)
	if n < 1000 {
		t.Errorf("look-ahead check covered only %d computed rows", n)
	}

	// 4. Re-importing the same files writes nothing and recomputes nothing.
	before := g.count(ctx, t, `SELECT count(*) FROM dp.datapoints`)
	runsBefore := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE kind = 'derive.cascade'`)
	again := g.importFile(ctx, t, "MultiAsset_Allocation_Framework_v2.xlsx", trackers, logs...)
	for k, v := range g.importFile(ctx, t, "extras.xlsx", extras, "upload.extras") {
		again[k] = v
	}
	for m, n := range again {
		if n != 0 {
			t.Errorf("re-import %s wrote %d rows", m, n)
		}
	}
	if after := g.count(ctx, t, `SELECT count(*) FROM dp.datapoints`); after != before {
		t.Errorf("re-import changed datapoints: %d → %d", before, after)
	}
	if runs := g.count(ctx, t, `SELECT count(*) FROM jobs.job_runs WHERE kind = 'derive.cascade'`); runs != runsBefore {
		t.Errorf("re-import started %d cascades", runs-runsBefore)
	}
}

// xlsx writes a one-sheet workbook. Numeric cells are stored as numbers,
// everything else as shared strings; empty cells are omitted, as formula
// cells with no cached value are in a real export.
func xlsx(t *testing.T, sheet string, rows [][]string) []byte {
	t.Helper()
	var shared []string
	idx := map[string]int{}
	var sb strings.Builder
	sb.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sb, `<row r="%d">`, r+1)
		for c, v := range row {
			if v == "" {
				continue
			}
			ref := colName(c) + strconv.Itoa(r+1)
			if _, err := strconv.ParseFloat(v, 64); err == nil {
				fmt.Fprintf(&sb, `<c r="%s"><v>%s</v></c>`, ref, v)
				continue
			}
			i, ok := idx[v]
			if !ok {
				i = len(shared)
				idx[v] = i
				shared = append(shared, v)
			}
			fmt.Fprintf(&sb, `<c r="%s" t="s"><v>%d</v></c>`, ref, i)
		}
		sb.WriteString(`</row>`)
	}
	sb.WriteString(`</sheetData></worksheet>`)
	var ss strings.Builder
	ss.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, s := range shared {
		ss.WriteString(`<si><t>`)
		_ = xml.EscapeText(&ss, []byte(s))
		ss.WriteString(`</t></si>`)
	}
	ss.WriteString(`</sst>`)
	var name strings.Builder
	_ = xml.EscapeText(&name, []byte(sheet))
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`},
		{"xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` +
			`<sheet name="` + name.String() + `" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`},
		{"xl/sharedStrings.xml", ss.String()},
		{"xl/worksheets/sheet1.xml", sb.String()},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// colName is the spreadsheet column for a 0-based index: A … Z, AA, AB, ….
func colName(c int) string {
	s := ""
	for c++; c > 0; c = (c - 1) / 26 {
		s = string(rune('A'+(c-1)%26)) + s
	}
	return s
}
