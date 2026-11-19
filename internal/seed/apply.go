package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/shubham-kukreja/core-jobs-service/internal/audit"
	"github.com/shubham-kukreja/core-jobs-service/internal/catalog"
	"github.com/shubham-kukreja/core-jobs-service/internal/ingest"
	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/database"
)

// Rebuilder enqueues derive.cascade in rebuild mode inside tx.
type Rebuilder interface {
	EnqueueRebuild(ctx context.Context, tx pgx.Tx, keys []string, actorID int64) (jobs.RunRef, error)
}

// Target is where Apply writes.
type Target struct {
	DB       database.TxBeginner
	Catalog  catalog.Writer
	Mappings ingest.Mappings
	Audit    audit.Auditor
	Rebuilds Rebuilder
}

// Report says what Apply wrote.
type Report struct {
	Sources  int `json:"sources"`
	Entities int `json:"entities"`
	Series   int `json:"series"`
	Mappings int `json:"mappings"`
	// Rebuild lists series whose derivation is new or changed; one
	// derive.cascade rebuild covers them and everything downstream.
	Rebuild []string     `json:"rebuild,omitempty"`
	Run     *jobs.RunRef `json:"-"`
}

// Apply writes sources, entities, series and import mappings from validated
// seed docs, as actorID. Everything up to the series commits in one
// transaction with its audit row and the rebuild job, so a bad seed changes
// nothing. Mappings are saved after, since they validate against committed
// series. Applying the same seed twice changes nothing and enqueues nothing.
//
// Schedules are not applied: their kinds (veto.evaluate, model.run) are
// registered in Phase 3.
func Apply(ctx context.Context, t Target, docs Documents, actorID int64) (Report, error) {
	var rep Report
	sources, err := decode[[]sourceDoc](docs["sources.yaml"])
	if err != nil {
		return rep, fmt.Errorf("sources.yaml: %w", err)
	}
	entities, err := decode[[]entityDoc](docs["entities.yaml"])
	if err != nil {
		return rep, fmt.Errorf("entities.yaml: %w", err)
	}
	series, err := SeriesInputs(docs)
	if err != nil {
		return rep, err
	}
	maps, err := mappingDocs(docs)
	if err != nil {
		return rep, err
	}

	err = database.InTx(ctx, t.DB, func(tx pgx.Tx) error {
		for _, s := range sources {
			if _, err := t.Catalog.SaveSource(ctx, tx, catalog.SourceInput{
				Key: s.Key, Kind: catalog.SourceKind(s.Kind), Name: s.Name, Config: s.Config, Active: true,
			}); err != nil {
				return fmt.Errorf("source %s: %w", s.Key, err)
			}
			rep.Sources++
		}
		for _, e := range parentsFirst(entities) {
			if _, err := t.Catalog.SaveEntity(ctx, tx, catalog.Entity{
				Key: e.Key, Kind: e.Kind, Name: e.Name, ParentKey: e.Parent, Meta: e.Meta, Active: true,
			}); err != nil {
				return fmt.Errorf("entity %s: %w", e.Key, err)
			}
			rep.Entities++
		}
		for _, s := range series {
			res, err := t.Catalog.SaveSeries(ctx, tx, s)
			if err != nil {
				return fmt.Errorf("series %s: %w", s.Key, err)
			}
			if res.Rebuild {
				rep.Rebuild = append(rep.Rebuild, s.Key)
			}
			rep.Series++
		}
		if len(rep.Rebuild) > 0 && t.Rebuilds != nil {
			ref, err := t.Rebuilds.EnqueueRebuild(ctx, tx, rep.Rebuild, actorID)
			if err != nil {
				return fmt.Errorf("enqueue rebuild: %w", err)
			}
			rep.Run = &ref
		}
		return t.Audit.Record(ctx, tx, audit.Entry{
			ActorID: &actorID, ActorKind: audit.ActorUser, Entity: "seed", EntityID: "configs/seed",
			Action: "apply", After: rep,
		})
	})
	if err != nil {
		return Report{}, err
	}

	for _, m := range maps {
		if _, err := t.Mappings.Save(ctx, m.Key, m.Name, m.Mapping, actorID); err != nil {
			return rep, fmt.Errorf("import mapping %s: %w", m.Key, err)
		}
		rep.Mappings++
	}
	return rep, nil
}

type sourceDoc struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

type entityDoc struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Parent string         `json:"parent"`
	Meta   map[string]any `json:"meta"`
}

type mappingDoc struct {
	Key     string         `json:"key"`
	Name    string         `json:"name"`
	Mapping ingest.Mapping `json:"mapping"`
}

func mappingDocs(docs Documents) ([]mappingDoc, error) {
	var paths []string
	for p := range docs {
		if strings.HasPrefix(p, "import_mappings"+string(filepath.Separator)) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	out := make([]mappingDoc, 0, len(paths))
	for _, p := range paths {
		m, err := decode[mappingDoc](docs[p])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// decode re-reads a schema-checked YAML document into T, rejecting fields T
// does not know so a schema/struct mismatch fails loudly.
func decode[T any](doc any) (T, error) {
	var out T
	raw, err := json.Marshal(doc)
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err = dec.Decode(&out)
	return out, err
}

// parentsFirst orders entities so each parent is saved before its children.
func parentsFirst(es []entityDoc) []entityDoc {
	byKey := make(map[string]entityDoc, len(es))
	for _, e := range es {
		byKey[e.Key] = e
	}
	depth := func(e entityDoc) int {
		n := 0
		for seen := map[string]bool{}; e.Parent != "" && !seen[e.Key]; n++ {
			seen[e.Key] = true
			e = byKey[e.Parent]
		}
		return n
	}
	out := append([]entityDoc(nil), es...)
	sort.SliceStable(out, func(i, j int) bool { return depth(out[i]) < depth(out[j]) })
	return out
}
