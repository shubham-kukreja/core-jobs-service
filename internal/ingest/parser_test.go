package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

var ratesMapping = Mapping{
	Sheet:          "Data Trackers",
	HeaderRowMatch: "Month",
	DateColumn:     DateColumn{Name: "Month", Format: "Jan-2006", ObsDate: "month_end"},
	Columns: []ColumnMap{
		{Name: "10Y G-sec Yield %", Series: "yield.gsec10y"},
		{Name: "3Y AAA Corp Yield %", Series: "yield.aaa3y"},
	},
	IgnoreColumns: []string{"AAA Spread (bps)"},
	KnownAt:       "obs_date_plus_days:5",
}

const ratesCSV = "\xef\xbb\xbfData Trackers export,,,\n" +
	"Month,10Y G-sec Yield %,3Y AAA Corp Yield %,AAA Spread (bps)\n" +
	"Jan-2026,6.70,7.45,75\n" +
	",,,\n" +
	"Feb-2026,6.65,NA,\n" +
	"Mar-2026,abc,7.30,\n" +
	"Smarch-2026,6.60,7.20,\n"

// buildXLSX writes a minimal workbook: one shared-string table and one sheet.
func buildXLSX(t *testing.T, sheet string, rows [][]string, extra map[string]string) []byte {
	t.Helper()
	var shared []string
	idx := map[string]int{}
	var sb strings.Builder
	sb.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sb, `<row r="%d">`, r+1)
		for c, v := range row {
			ref := fmt.Sprintf("%c%d", 'A'+c, r+1)
			if v == "" {
				continue
			}
			if _, err := fmt.Sscanf(v, "%g", new(float64)); err == nil && !strings.ContainsAny(v, "-") {
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
		fmt.Fprintf(&ss, `<si><t>%s</t></si>`, s)
	}
	ss.WriteString(`</sst>`)
	parts := map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` +
			`<sheet name="Other" sheetId="1" r:id="rId2"/><sheet name="` + sheet + `" sheetId="2" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="worksheets/sheet2.xml"/></Relationships>`,
		"xl/sharedStrings.xml":     ss.String(),
		"xl/worksheets/sheet1.xml": sb.String(),
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData/></worksheet>`,
	}
	for k, v := range extra {
		parts[k] = v
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ratesGrid() [][]string {
	return [][]string{
		{"Data Trackers export"},
		{"Month", "10Y G-sec Yield %", "3Y AAA Corp Yield %", "AAA Spread (bps)"},
		{"46053", "6.70", "7.45", "75"}, // Excel serial for 31-Jan-2026
		{"Feb-2026", "6.65", "NA"},
		{"Mar-2026", "abc", "7.30"},
		{"Smarch-2026", "6.60", "7.20"},
	}
}

var now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func checkRates(t *testing.T, rows []Row) {
	t.Helper()
	if len(rows) != 4 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	pts, rej, err := ToPoints(rows, ratesMapping, now)
	if err != nil {
		t.Fatal(err)
	}
	// Jan: 2 points; Feb: 10Y only; Mar: AAA only (10Y rejected); Smarch rejected.
	if len(pts) != 4 || len(rej) != 2 {
		t.Fatalf("pts %d rej %+v", len(pts), rej)
	}
	jan := civil.Date{Year: 2026, Month: 1, Day: 31}
	if pts[0].ObsDate != jan || *pts[0].Value.Num != 6.70 || pts[0].SeriesKey != "yield.gsec10y" {
		t.Fatalf("first point %+v", pts[0])
	}
	if want := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC); !pts[0].KnownAt.Equal(want) {
		t.Fatalf("known_at %v, want %v", pts[0].KnownAt, want)
	}
	if rej[0].Reason != ReasonBadNumber || rej[1].Reason != ReasonBadDate {
		t.Fatalf("rejections %+v", rej)
	}
}

func TestParseCSV(t *testing.T) {
	rows, err := NewParser().Parse(context.Background(), strings.NewReader(ratesCSV), MediaCSV, ratesMapping, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	checkRates(t, rows)
}

func TestParseXLSX(t *testing.T) {
	data := buildXLSX(t, "Data Trackers", ratesGrid(), nil)
	if ct, err := sniff(data); err != nil || ct != MediaXLSX {
		t.Fatalf("sniff: %s %v", ct, err)
	}
	rows, err := NewParser().Parse(context.Background(), bytes.NewReader(data), MediaXLSX, ratesMapping, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	checkRates(t, rows)
}

func TestParseBadMapping(t *testing.T) {
	cases := map[string]Mapping{
		"no header": func() Mapping { m := ratesMapping; m.HeaderRowMatch = "Date"; return m }(),
		"missing col": func() Mapping {
			m := ratesMapping
			m.Columns = append([]ColumnMap{{Name: "Repo %", Series: "rate.repo"}}, m.Columns...)
			return m
		}(),
		"unignored col": func() Mapping { m := ratesMapping; m.IgnoreColumns = nil; return m }(),
	}
	for name, m := range cases {
		_, err := NewParser().Parse(context.Background(), strings.NewReader(ratesCSV), MediaCSV, m, DefaultLimits)
		if !errors.Is(err, ErrBadMapping) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	m := ratesMapping
	m.Sheet = "Missing"
	data := buildXLSX(t, "Data Trackers", ratesGrid(), nil)
	if _, err := NewParser().Parse(context.Background(), bytes.NewReader(data), MediaXLSX, m, DefaultLimits); !errors.Is(err, ErrBadMapping) {
		t.Errorf("missing sheet: %v", err)
	}
}

func TestParseLimits(t *testing.T) {
	small := DefaultLimits
	small.MaxRows = 3
	if _, err := NewParser().Parse(context.Background(), strings.NewReader(ratesCSV), MediaCSV, ratesMapping, small); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("csv rows: %v", err)
	}
	data := buildXLSX(t, "Data Trackers", ratesGrid(), nil)
	if _, err := NewParser().Parse(context.Background(), bytes.NewReader(data), MediaXLSX, ratesMapping, small); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("xlsx rows: %v", err)
	}
	// A highly compressible part inflating past the budget (zip bomb).
	bomb := buildXLSX(t, "Data Trackers", ratesGrid(), map[string]string{"xl/sharedStrings.xml": "<sst>" + strings.Repeat(" ", 2<<20) + "</sst>"})
	tiny := DefaultLimits
	tiny.MaxUncompressedSize = 1 << 20
	if _, err := NewParser().Parse(context.Background(), bytes.NewReader(bomb), MediaXLSX, ratesMapping, tiny); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("zip bomb: %v", err)
	}
	if _, err := NewParser().Parse(context.Background(), strings.NewReader("x"), "application/pdf", ratesMapping, DefaultLimits); !errors.Is(err, ErrUnsupportedMedia) {
		t.Errorf("pdf: %v", err)
	}
}

func TestSniffAndSanitize(t *testing.T) {
	if _, err := sniff([]byte("PK\x03\x04garbage")); err == nil {
		t.Error("broken zip accepted")
	}
	if _, err := sniff([]byte{0xff, 0x00, 0x01}); err == nil {
		t.Error("binary accepted")
	}
	for in, want := range map[string]string{
		"../../etc/passwd":       "passwd",
		`C:\Users\x\rates.csv`:   "rates.csv",
		"a\x00b\nc.csv":          "abc.csv",
		"":                       "upload",
		strings.Repeat("é", 200): strings.Repeat("é", 127),
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateMappingAndKnownAt(t *testing.T) {
	if err := ValidateMapping(ratesMapping); err != nil {
		t.Fatal(err)
	}
	bad := ratesMapping
	bad.KnownAt = "tomorrow"
	if err := ValidateMapping(bad); err == nil {
		t.Error("bad known_at accepted")
	}
	bad = ratesMapping
	bad.IgnoreColumns = []string{"Month"}
	if err := ValidateMapping(bad); err == nil {
		t.Error("ignored date column accepted")
	}
	rule, _ := knownAtRule("obs_date_plus_days:5")
	if got := rule(civil.Date{Year: 2026, Month: 5, Day: 31}, now); !got.Equal(now) {
		t.Errorf("known_at must not pass now: %v", got)
	}
}
