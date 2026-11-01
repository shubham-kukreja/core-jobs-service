// Package ingest brings external data in: uploaded files mapped to series
// (import.file) and, from Phase 5, source connectors (ingest.fetch). All
// values are written through datapoints.Writer.
package ingest

import (
	"context"
	"errors"
	"io"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/uuid"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
)

// Accepted upload media types. Detection is by content, not filename.
const (
	MediaCSV  = "text/csv"
	MediaXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// Upload is a stored file awaiting import.
type Upload struct {
	ID          int64
	UUID        uuid.UUID
	Filename    string // sanitised display name; never used as a path
	ContentType string
	Bytes       int64
	SHA256      []byte
	BlobURI     string
	UploadedBy  int64
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}

// UploadInput is a file being uploaded.
type UploadInput struct {
	Filename   string
	Body       io.Reader
	UploadedBy int64
}

// Uploads stores files. Store enforces the size limit, sniffs the content
// type, and rejects anything that is not CSV or XLSX.
type Uploads interface {
	Store(ctx context.Context, in UploadInput) (Upload, error)
	Get(ctx context.Context, id uuid.UUID) (Upload, error)
	Open(ctx context.Context, id uuid.UUID) (io.ReadCloser, Upload, error)
}

// DateColumn says how to read the observation date.
type DateColumn struct {
	Name    string `json:"name"`
	Format  string `json:"format"`             // Go layout, e.g. "Jan-2006"
	ObsDate string `json:"obs_date,omitempty"` // "" (as is) | "month_end"
}

// ColumnMap maps one file column to a series.
type ColumnMap struct {
	Name   string `json:"name"`
	Series string `json:"series"`
}

// Mapping describes how one file layout maps to series.
type Mapping struct {
	Sheet          string      `json:"sheet,omitempty"` // XLSX only
	HeaderRowMatch string      `json:"header_row_match"`
	DateColumn     DateColumn  `json:"date_column"`
	Columns        []ColumnMap `json:"columns"`
	IgnoreColumns  []string    `json:"ignore_columns,omitempty"`
	// KnownAt rule: "now" | "obs_date_plus_days:<n>".
	KnownAt string `json:"known_at"`
}

// ImportMapping is a saved, versioned mapping.
type ImportMapping struct {
	ID        int64
	UUID      uuid.UUID
	Key       string
	Name      string
	Version   int
	Mapping   Mapping
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Mappings stores import mappings. Save validates every referenced series
// exists and is writable by file import.
type Mappings interface {
	Save(ctx context.Context, key, name string, m Mapping, actorID int64) (ImportMapping, error)
	Get(ctx context.Context, key string) (ImportMapping, error)
	List(ctx context.Context) ([]ImportMapping, error)
}

// Limits bound parser resource use (defence against zip bombs and huge
// sheets). Exceeding any limit fails the parse.
type Limits struct {
	MaxRows             int
	MaxColumns          int
	MaxCellBytes        int
	MaxUncompressedSize int64 // XLSX: total inflated bytes
	MaxSheets           int
}

// Row is one parsed row: header name → raw cell text. Formulas are never
// evaluated; only cached values are read.
type Row struct {
	Line  int
	Cells map[string]string
}

// Parser turns a file into rows under a mapping.
type Parser interface {
	Parse(ctx context.Context, r io.Reader, contentType string, m Mapping, lim Limits) ([]Row, error)
}

// ImportResult is the outcome of import.file.
type ImportResult struct {
	RowsWritten int
	RowsSkipped int
	Rejected    []datapoints.Rejection
	BatchID     uuid.UUID
	Preview     []datapoints.Point // dry run only
}

// FetchRequest asks a connector for a date range of its series.
type FetchRequest struct {
	SeriesKeys []string
	From, To   civil.Date
}

// FetchResult is what a connector returns. Raw responses are stored by the
// framework, not the connector.
type FetchResult struct {
	Points []datapoints.Point
	Raw    []RawResponse
}

// RawResponse is one upstream response. Request must not contain credentials.
type RawResponse struct {
	Request     map[string]any
	Body        []byte
	ContentType string
	FetchedAt   time.Time
}

// Connector fetches one external source. Credentials come from the
// environment by reference (catalog.sources.config.credential_ref).
type Connector interface {
	Key() string
	Series() []string
	Fetch(ctx context.Context, req FetchRequest) (FetchResult, error)
}

// ConnectorRegistry holds connectors by key.
type ConnectorRegistry interface {
	Register(c Connector)
	Get(key string) (Connector, bool)
	Keys() []string
}

var (
	ErrNotFound         = errors.New("ingest: not found")
	ErrUnsupportedMedia = errors.New("ingest: unsupported media type")
	ErrTooLarge         = errors.New("ingest: file too large")
	ErrLimitExceeded    = errors.New("ingest: parse limit exceeded")
	ErrBadMapping       = errors.New("ingest: mapping does not match file")
)
