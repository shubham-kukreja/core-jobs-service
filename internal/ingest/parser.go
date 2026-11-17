package ingest

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/civil"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/calendar"
)

// DefaultLimits bound a tracker file comfortably (10 years × a few hundred
// columns) while stopping hostile files early.
var DefaultLimits = Limits{
	MaxRows:             20_000,
	MaxColumns:          500,
	MaxCellBytes:        4_096,
	MaxUncompressedSize: 100 << 20,
	MaxSheets:           50,
}

type parser struct{}

// NewParser returns the CSV and XLSX parser.
func NewParser() Parser { return parser{} }

// Parse reads the file, finds the header row (the first row with a cell equal
// to m.HeaderRowMatch), checks every mapped column exists and every other
// column is ignored, and returns the data rows keyed by header name. Only the
// date column and mapped columns are kept.
func (parser) Parse(ctx context.Context, r io.Reader, contentType string, m Mapping, lim Limits) ([]Row, error) {
	data, err := io.ReadAll(io.LimitReader(r, lim.MaxUncompressedSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > lim.MaxUncompressedSize {
		return nil, fmt.Errorf("%w: file over %d bytes", ErrLimitExceeded, lim.MaxUncompressedSize)
	}
	var grid [][]string
	switch contentType {
	case MediaCSV:
		grid, err = readCSV(data, lim)
	case MediaXLSX:
		grid, err = readXLSX(data, m.Sheet, lim)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedMedia, contentType)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mapGrid(grid, m)
}

func readCSV(data []byte, lim Limits) ([][]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // Excel's UTF-8 BOM
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: csv is not UTF-8", ErrUnsupportedMedia)
	}
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = false
	var grid [][]string
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return grid, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: csv: %w", ErrUnsupportedMedia, err)
		}
		if lim.MaxRows > 0 && len(grid) >= lim.MaxRows {
			return nil, fmt.Errorf("%w: more than %d rows", ErrLimitExceeded, lim.MaxRows)
		}
		if lim.MaxColumns > 0 && len(rec) > lim.MaxColumns {
			return nil, fmt.Errorf("%w: more than %d columns", ErrLimitExceeded, lim.MaxColumns)
		}
		for _, c := range rec {
			if lim.MaxCellBytes > 0 && len(c) > lim.MaxCellBytes {
				return nil, fmt.Errorf("%w: cell over %d bytes", ErrLimitExceeded, lim.MaxCellBytes)
			}
		}
		grid = append(grid, rec)
	}
}

func mapGrid(grid [][]string, m Mapping) ([]Row, error) {
	hdr := -1
	for i, row := range grid {
		for _, c := range row {
			if strings.TrimSpace(c) == m.HeaderRowMatch {
				hdr = i
				break
			}
		}
		if hdr >= 0 {
			break
		}
	}
	if hdr < 0 {
		return nil, fmt.Errorf("%w: no header row containing %q", ErrBadMapping, m.HeaderRowMatch)
	}
	want := map[string]bool{m.DateColumn.Name: true}
	for _, c := range m.Columns {
		want[c.Name] = true
	}
	ignored := map[string]bool{}
	for _, c := range m.IgnoreColumns {
		ignored[c] = true
	}
	idx := map[string]int{}
	for i, h := range grid[hdr] {
		h = strings.TrimSpace(h)
		switch {
		case h == "":
		case want[h]:
			if _, dup := idx[h]; dup {
				return nil, fmt.Errorf("%w: column %q appears twice", ErrBadMapping, h)
			}
			idx[h] = i
		case !ignored[h]:
			return nil, fmt.Errorf("%w: column %q is neither mapped nor ignored", ErrBadMapping, h)
		}
	}
	for name := range want {
		if _, ok := idx[name]; !ok {
			return nil, fmt.Errorf("%w: column %q not in file", ErrBadMapping, name)
		}
	}
	var rows []Row
	for i := hdr + 1; i < len(grid); i++ {
		cells := map[string]string{}
		empty := true
		for name, j := range idx {
			if j < len(grid[i]) {
				v := strings.TrimSpace(grid[i][j])
				cells[name] = v
				if v != "" {
					empty = false
				}
			}
		}
		if !empty {
			rows = append(rows, Row{Line: i + 1, Cells: cells})
		}
	}
	return rows, nil
}

// Reasons for rows the importer rejects before they reach the writer.
const (
	ReasonBadDate   = "invalid_date"
	ReasonBadNumber = "invalid_number"
)

// ToPoints converts parsed rows to datapoints. Blank and "NA"/"-" cells are
// skipped; a cell that cannot be read is rejected with its line, never fatal.
// Rejection.Index is the file line.
func ToPoints(rows []Row, m Mapping, now time.Time) ([]datapoints.Point, []datapoints.Rejection, error) {
	known, err := knownAtRule(m.KnownAt)
	if err != nil {
		return nil, nil, err
	}
	var pts []datapoints.Point
	var rej []datapoints.Rejection
	for _, r := range rows {
		d, err := parseDate(r.Cells[m.DateColumn.Name], m.DateColumn)
		if err != nil {
			rej = append(rej, datapoints.Rejection{Index: r.Line, Reason: ReasonBadDate,
				Message: fmt.Sprintf("line %d: %v", r.Line, err)})
			continue
		}
		for _, c := range m.Columns {
			raw := r.Cells[c.Name]
			if skipCell(raw) {
				continue
			}
			f, err := parseNumber(raw)
			if err != nil {
				rej = append(rej, datapoints.Rejection{Index: r.Line, SeriesKey: c.Series, Reason: ReasonBadNumber,
					Message: fmt.Sprintf("line %d, %s: %q is not a number", r.Line, c.Name, raw)})
				continue
			}
			pts = append(pts, datapoints.Point{SeriesKey: c.Series, ObsDate: d, Value: datapoints.Num(f), KnownAt: known(d, now)})
		}
	}
	return pts, rej, nil
}

func skipCell(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "-", "NA", "N/A", "#N/A":
		return true
	}
	return false
}

func parseNumber(s string) (float64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	s = strings.TrimSuffix(s, "%")
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("not a number")
	}
	return f, nil
}

// excelEpoch is day 0 of Excel's 1900 date system (with its leap-year bug).
var excelEpoch = civil.Date{Year: 1899, Month: time.December, Day: 30}

func parseDate(s string, dc DateColumn) (civil.Date, error) {
	s = strings.TrimSpace(s)
	var d civil.Date
	if t, err := time.Parse(dc.Format, s); err == nil {
		d = civil.DateOf(t)
	} else if serial, err2 := strconv.ParseFloat(s, 64); err2 == nil && serial >= 1 && serial < 2958466 {
		d = excelEpoch.AddDays(int(serial)) // XLSX stores dates as serial numbers
	} else {
		return civil.Date{}, fmt.Errorf("date %q does not match %q", s, dc.Format)
	}
	if dc.ObsDate == "month_end" {
		d = calendar.MonthEnd(d)
	}
	return d, nil
}

// knownAtRule returns the function that stamps known_at: "now" or
// "obs_date_plus_days:<n>" (an estimate of the source's publication lag),
// never later than now.
func knownAtRule(rule string) (func(civil.Date, time.Time) time.Time, error) {
	if rule == "now" {
		return func(_ civil.Date, now time.Time) time.Time { return now }, nil
	}
	n, ok := strings.CutPrefix(rule, "obs_date_plus_days:")
	days, err := strconv.Atoi(n)
	if !ok || err != nil || days < 0 || days > 999 {
		return nil, fmt.Errorf("%w: known_at rule %q", ErrBadMapping, rule)
	}
	return func(d civil.Date, now time.Time) time.Time {
		t := d.AddDays(days).In(time.UTC)
		if t.After(now) {
			return now
		}
		return t
	}, nil
}
