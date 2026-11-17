package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/apperr"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/blob"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// ---- uploads ---------------------------------------------------------------

type uploads struct {
	db       *pgxpool.Pool
	blobs    blob.Store
	clk      clock.Clock
	maxBytes int64
}

// NewUploads returns the upload store. maxBytes caps one file.
func NewUploads(db *pgxpool.Pool, blobs blob.Store, clk clock.Clock, maxBytes int64) Uploads {
	return &uploads{db: db, blobs: blobs, clk: clk, maxBytes: maxBytes}
}

const uploadCols = `id, uuid, filename, content_type, bytes, sha256, blob_uri, uploaded_by, created_at, expires_at`

func scanUpload(row pgx.Row) (Upload, error) {
	var u Upload
	err := row.Scan(&u.ID, &u.UUID, &u.Filename, &u.ContentType, &u.Bytes, &u.SHA256, &u.BlobURI, &u.UploadedBy, &u.CreatedAt, &u.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, apperr.Wrap(ErrNotFound, apperr.NotFound, "upload not found")
	}
	return u, err
}

// Store buffers the file (bounded by maxBytes), detects its type from the
// content, and stores it encrypted in the blob store before recording it.
func (s *uploads) Store(ctx context.Context, in UploadInput) (Upload, error) {
	data, err := io.ReadAll(io.LimitReader(in.Body, s.maxBytes+1))
	if err != nil {
		return Upload{}, fmt.Errorf("ingest: read upload: %w", err)
	}
	if int64(len(data)) > s.maxBytes {
		return Upload{}, apperr.Wrap(ErrTooLarge, apperr.ValidationFailed, fmt.Sprintf("file is larger than %d bytes", s.maxBytes))
	}
	if len(data) == 0 {
		return Upload{}, apperr.Wrap(ErrUnsupportedMedia, apperr.ValidationFailed, "file is empty")
	}
	ct, err := sniff(data)
	if err != nil {
		return Upload{}, apperr.Wrap(err, apperr.ValidationFailed, "only CSV and XLSX files are accepted")
	}
	id := uuid.New()
	obj, err := s.blobs.Put(ctx, "uploads/"+id.String(), ct, bytes.NewReader(data), s.maxBytes)
	if err != nil {
		return Upload{}, fmt.Errorf("ingest: store upload: %w", err)
	}
	sum := sha256.Sum256(data)
	u, err := scanUpload(s.db.QueryRow(ctx, `INSERT INTO ingest.uploads(uuid, filename, content_type, bytes, sha256, blob_uri, uploaded_by)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+uploadCols,
		id, sanitizeFilename(in.Filename), ct, int64(len(data)), sum[:], obj.URI, in.UploadedBy))
	if err != nil {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), obj.URI)
		return Upload{}, fmt.Errorf("ingest: record upload: %w", err)
	}
	return u, nil
}

func (s *uploads) Get(ctx context.Context, id uuid.UUID) (Upload, error) {
	return scanUpload(s.db.QueryRow(ctx, `SELECT `+uploadCols+` FROM ingest.uploads WHERE uuid = $1`, id))
}

// Open returns the file body after checking it still matches its sha256.
func (s *uploads) Open(ctx context.Context, id uuid.UUID) (io.ReadCloser, Upload, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, u, err
	}
	rc, _, err := s.blobs.Get(ctx, u.BlobURI)
	if err != nil {
		return nil, u, fmt.Errorf("ingest: open upload: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, u.Bytes+1))
	if err != nil {
		return nil, u, fmt.Errorf("ingest: read upload: %w", err)
	}
	if sum := sha256.Sum256(data); !bytes.Equal(sum[:], u.SHA256) {
		return nil, u, fmt.Errorf("ingest: upload %s failed its integrity check", id)
	}
	return io.NopCloser(bytes.NewReader(data)), u, nil
}

// sniff detects CSV or XLSX from content. An XLSX is a zip with a workbook
// part; a CSV is UTF-8 text without NUL bytes. Anything else is rejected.
func sniff(data []byte) (string, error) {
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", ErrUnsupportedMedia
		}
		for _, f := range zr.File {
			if f.Name == "xl/workbook.xml" {
				return MediaXLSX, nil
			}
		}
		return "", ErrUnsupportedMedia
	}
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return MediaCSV, nil
	}
	return "", ErrUnsupportedMedia
}

// sanitizeFilename keeps a display name only: no directories, no control
// characters, at most 255 bytes.
func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > 255 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." || name == "/" {
		return "upload"
	}
	return name
}

// ---- mappings ----------------------------------------------------------------

// SeriesLookup is the catalog read the mapping store needs.
type SeriesLookup interface {
	Series(ctx context.Context, key string) (catalog.Series, error)
}

type mappings struct {
	db  *pgxpool.Pool
	cat SeriesLookup
}

// NewMappings returns the import mapping store.
func NewMappings(db *pgxpool.Pool, cat SeriesLookup) Mappings { return &mappings{db: db, cat: cat} }

const mappingCols = `id, uuid, key, name, version, mapping, created_by, created_at, updated_at`

func scanMapping(row pgx.Row) (ImportMapping, error) {
	var m ImportMapping
	var raw []byte
	err := row.Scan(&m.ID, &m.UUID, &m.Key, &m.Name, &m.Version, &raw, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, apperr.Wrap(ErrNotFound, apperr.NotFound, "import mapping not found")
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(raw, &m.Mapping)
}

// ValidateMapping checks a mapping's shape (not its series).
func ValidateMapping(m Mapping) error {
	bad := func(msg string) error { return apperr.Wrap(ErrBadMapping, apperr.ValidationFailed, msg) }
	if strings.TrimSpace(m.HeaderRowMatch) == "" {
		return bad("header_row_match is required")
	}
	if m.DateColumn.Name == "" || m.DateColumn.Format == "" {
		return bad("date_column needs name and format")
	}
	if m.DateColumn.ObsDate != "" && m.DateColumn.ObsDate != "month_end" {
		return bad("date_column.obs_date must be empty or month_end")
	}
	if len(m.Columns) == 0 || len(m.Columns) > 500 {
		return bad("columns must list 1 to 500 columns")
	}
	if _, err := knownAtRule(m.KnownAt); err != nil {
		return bad("known_at must be now or obs_date_plus_days:<n>")
	}
	seen := map[string]bool{m.DateColumn.Name: true}
	for _, c := range m.Columns {
		if c.Name == "" || c.Series == "" {
			return bad("every column needs name and series")
		}
		if seen[c.Name] {
			return bad(fmt.Sprintf("column %q is listed twice", c.Name))
		}
		seen[c.Name] = true
	}
	for _, c := range m.IgnoreColumns {
		if seen[c] {
			return bad(fmt.Sprintf("column %q is both mapped and ignored", c))
		}
	}
	return nil
}

// Save validates and upserts a mapping; changing an existing key bumps its
// version. Every series must exist, be active, numeric and raw (the only
// layer file import may write).
func (s *mappings) Save(ctx context.Context, key, name string, m Mapping, actorID int64) (ImportMapping, error) {
	if err := ValidateMapping(m); err != nil {
		return ImportMapping{}, err
	}
	for _, c := range m.Columns {
		ser, err := s.cat.Series(ctx, c.Series)
		if errors.Is(err, catalog.ErrNotFound) {
			return ImportMapping{}, apperr.Wrap(ErrBadMapping, apperr.ValidationFailed, fmt.Sprintf("series %q does not exist", c.Series))
		}
		if err != nil {
			return ImportMapping{}, err
		}
		if !ser.Active || ser.Layer != catalog.LayerRaw || ser.ValueType != catalog.ValueNum {
			return ImportMapping{}, apperr.Wrap(ErrBadMapping, apperr.ValidationFailed,
				fmt.Sprintf("series %q is not an active numeric raw series", c.Series))
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ImportMapping{}, err
	}
	im, err := scanMapping(s.db.QueryRow(ctx, `INSERT INTO ingest.import_mappings(key, name, mapping, created_by)
VALUES ($1,$2,$3,$4)
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, mapping = EXCLUDED.mapping,
  version = ingest.import_mappings.version + 1, updated_at = now()
  WHERE ingest.import_mappings.mapping IS DISTINCT FROM EXCLUDED.mapping OR ingest.import_mappings.name IS DISTINCT FROM EXCLUDED.name
RETURNING `+mappingCols, key, name, raw, actorID))
	if errors.Is(err, ErrNotFound) { // unchanged: the upsert's WHERE skipped the update
		return s.Get(ctx, key)
	}
	return im, err
}

func (s *mappings) Get(ctx context.Context, key string) (ImportMapping, error) {
	return scanMapping(s.db.QueryRow(ctx, `SELECT `+mappingCols+` FROM ingest.import_mappings WHERE key = $1`, key))
}

func (s *mappings) List(ctx context.Context) ([]ImportMapping, error) {
	rows, err := s.db.Query(ctx, `SELECT `+mappingCols+` FROM ingest.import_mappings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- connectors --------------------------------------------------------------

type connectors struct {
	mu sync.RWMutex
	m  map[string]Connector
}

// NewConnectorRegistry returns an empty registry. Phase 5 adds connectors.
func NewConnectorRegistry() ConnectorRegistry { return &connectors{m: map[string]Connector{}} }

// Register adds c; a duplicate key panics at boot.
func (r *connectors) Register(c Connector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[c.Key()]; dup {
		panic("ingest: duplicate connector " + c.Key())
	}
	r.m[c.Key()] = c
}

func (r *connectors) Get(key string) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[key]
	return c, ok
}

func (r *connectors) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
