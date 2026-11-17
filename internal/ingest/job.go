package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/datapoints"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// KindImportFile is Job 1.
const KindImportFile = "import.file"

// ImportArgs are import.file's arguments. MappingID is the mapping key.
type ImportArgs struct {
	UploadID  uuid.UUID `json:"upload_id"`
	MappingID string    `json:"mapping_id"`
	DryRun    bool      `json:"dry_run,omitempty"`
}

func (ImportArgs) Kind() string { return KindImportFile }

// ImportSpec is import.file's static definition.
var ImportSpec = jobs.Spec{
	Kind:            KindImportFile,
	Queue:           jobs.QueueIngest,
	Triggers:        []jobs.TriggerType{jobs.TriggerUser, jobs.TriggerSchedule},
	UserTriggerable: true,
	Roles:           []string{"analyst", "admin"},
	MaxAttempts:     3,
	Timeout:         10 * time.Minute,
	ArgsVersion:     1,
}

// Importer runs import.file.
type Importer struct {
	DB       database.TxBeginner
	Uploads  Uploads
	Mappings Mappings
	Parser   Parser
	Writer   datapoints.Writer
	Clock    clock.Clock
	Limits   Limits
}

const (
	maxReportedRejections = 200
	maxPreview            = 100
)

var errDryRun = errors.New("ingest: dry run rollback")

// Import parses the upload under the mapping and writes one batch. A dry run
// performs the same write, with full validation and no-op detection, inside
// a transaction it then rolls back, so it writes nothing (no rows, no outbox
// event). Bad rows are rejected and reported; a bad mapping or file fails
// permanently.
func (im *Importer) Import(ctx context.Context, a ImportArgs, jobRunID int64) (ImportResult, error) {
	m, err := im.Mappings.Get(ctx, a.MappingID)
	if err != nil {
		return ImportResult{}, jobs.Permanent(err)
	}
	body, up, err := im.Uploads.Open(ctx, a.UploadID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ImportResult{}, jobs.Permanent(err)
		}
		return ImportResult{}, err
	}
	defer func() { _ = body.Close() }()
	rows, err := im.Parser.Parse(ctx, body, up.ContentType, m.Mapping, im.Limits)
	if err != nil {
		return ImportResult{}, jobs.Permanent(err)
	}
	pts, rejected, err := ToPoints(rows, m.Mapping, im.Clock.Now())
	if err != nil {
		return ImportResult{}, jobs.Permanent(err)
	}
	res := ImportResult{Rejected: rejected}
	if len(pts) == 0 {
		return res, nil
	}
	meta := datapoints.WriteMeta{Source: "file_import", Origin: datapoints.OriginPrimary}
	if jobRunID != 0 {
		meta.JobRunID = &jobRunID
	}
	err = database.InTx(ctx, im.DB, func(tx pgx.Tx) error {
		w, err := im.Writer.Write(ctx, tx, meta, pts)
		if err != nil {
			return err
		}
		res.RowsWritten, res.RowsSkipped = w.Written, w.Skipped
		for _, r := range w.Rejected {
			r.Message = fmt.Sprintf("%s %s: %s", r.SeriesKey, pts[r.Index].ObsDate, r.Message)
			r.Index = -1 // a point index means nothing to the user
			res.Rejected = append(res.Rejected, r)
		}
		if a.DryRun {
			res.Preview = pts[:min(len(pts), maxPreview)]
			return errDryRun
		}
		res.BatchID = w.BatchID
		return nil
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	return res, err
}

// RegisterImport registers import.file.
func RegisterImport(r *jobs.Registry, im *Importer) {
	jobs.Register(r, ImportSpec, jobs.HandlerFunc[ImportArgs](func(ctx context.Context, rc *jobs.RunCtx, a ImportArgs) (jobs.Outcome, error) {
		res, err := im.Import(ctx, a, rc.RunID)
		if err != nil {
			return jobs.Outcome{}, err
		}
		rej := res.Rejected
		if len(rej) > maxReportedRejections {
			rej = rej[:maxReportedRejections]
		}
		sum := map[string]any{
			"rows_written": res.RowsWritten, "rows_skipped": res.RowsSkipped,
			"rejected_count": len(res.Rejected), "rejected": rej, "dry_run": a.DryRun,
		}
		if res.BatchID != uuid.Nil {
			sum["batch_id"] = res.BatchID
		}
		if a.DryRun {
			sum["preview"] = res.Preview
		}
		return jobs.Outcome{Summary: sum}, nil
	}))
}
