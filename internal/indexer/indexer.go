package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// Defaults.
const (
	DefaultBatchSize    = 1000
	DefaultFullInterval = time.Hour
	// MaxDeleteFraction guards against wiping a type when a source query returns nothing by
	// mistake: a full sync that would delete more than this fraction is refused (see Force).
	MaxDeleteFraction = 0.5
)

// ErrDeleteGuard means a full sync was refused because it would delete too many objects.
var ErrDeleteGuard = errors.New("full sync would delete too many objects")

// Indexer syncs datasource rows into object tables.
type Indexer struct {
	DB      *pgxpool.Pool // ontology database (object tables + indexer.sync_state)
	Store   *objstore.Store
	Sources map[string]Querier // connector name -> source database
	// FullInterval is how often a type with a timestampColumn also gets a full snapshot
	// (to catch hard deletes). Types without a timestampColumn always sync fully.
	FullInterval time.Duration
	BatchSize    int
	// Force disables the mass-delete guard for the next full syncs.
	Force bool
}

// Result reports one source's sync.
type Result struct {
	Source string             `json:"source"`
	Mode   string             `json:"mode"` // incremental | full
	Stats  objstore.SyncStats `json:"stats"`
	Error  string             `json:"error,omitempty"`
}

type cursor struct {
	ts       *time.Time
	pk       string
	lastFull *time.Time
}

func (ix *Indexer) batch() int {
	if ix.BatchSize <= 0 {
		return DefaultBatchSize
	}
	return ix.BatchSize
}

func (ix *Indexer) loadCursor(ctx context.Context, ont, source string) (cursor, error) {
	var c cursor
	var pk *string
	err := ix.DB.QueryRow(ctx, `SELECT cursor_ts, cursor_pk, last_full_sync FROM indexer.sync_state
WHERE ontology = $1 AND source = $2`, ont, source).Scan(&c.ts, &pk, &c.lastFull)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor{}, nil
	}
	if pk != nil {
		c.pk = *pk
	}
	return c, err
}

func saveCursor(ctx context.Context, tx pgx.Tx, ont, source string, c cursor, full bool) error {
	_, err := tx.Exec(ctx, `
INSERT INTO indexer.sync_state (ontology, source, cursor_ts, cursor_pk, last_full_sync, last_run_at, last_error)
VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN now() END, now(), NULL)
ON CONFLICT (ontology, source) DO UPDATE SET cursor_ts = EXCLUDED.cursor_ts, cursor_pk = EXCLUDED.cursor_pk,
  last_full_sync = COALESCE(EXCLUDED.last_full_sync, indexer.sync_state.last_full_sync),
  last_run_at = now(), last_error = NULL`, ont, source, c.ts, c.pk, full)
	return err
}

func (ix *Indexer) recordRun(ctx context.Context, ont, source string, stats objstore.SyncStats, runErr error) {
	statsJSON, _ := json.Marshal(stats)
	var msg *string
	if runErr != nil {
		s := runErr.Error()
		msg = &s
	}
	if _, err := ix.DB.Exec(ctx, `
INSERT INTO indexer.sync_state (ontology, source, last_run_at, last_error, stats) VALUES ($1, $2, now(), $3, $4)
ON CONFLICT (ontology, source) DO UPDATE SET last_run_at = now(), last_error = EXCLUDED.last_error, stats = EXCLUDED.stats`,
		ont, source, msg, statsJSON); err != nil {
		slog.WarnContext(ctx, "indexer: cannot record run", "source", source, "err", err)
	}
}

func (ix *Indexer) source(connector string) (Querier, error) {
	q, ok := ix.Sources[connector]
	if !ok {
		return nil, fmt.Errorf("connector %q has no source connection configured", connector)
	}
	return q, nil
}

// SyncObjectType syncs one object type: incrementally when it has a timestamp column and a full
// snapshot is not yet due, otherwise as a full snapshot.
func (ix *Indexer) SyncObjectType(ctx context.Context, ont *spec.CompiledOntology, typ string, forceFull bool) (Result, error) {
	source := "object:" + typ
	res := Result{Source: source}
	ot := ont.ObjectTypes[typ]
	if ot == nil || ot.Datasource == nil {
		return res, fmt.Errorf("object type %q has no datasource", typ)
	}
	plan, err := planFor(ot)
	if err != nil {
		return res, err
	}
	q, err := ix.source(ot.Datasource.Connector)
	if err != nil {
		return res, err
	}
	cur, err := ix.loadCursor(ctx, ont.APIName, source)
	if err != nil {
		return res, err
	}
	interval := ix.FullInterval
	if interval <= 0 {
		interval = DefaultFullInterval
	}
	full := forceFull || plan.tsCol == "" || cur.lastFull == nil || time.Since(*cur.lastFull) > interval
	if full {
		res.Mode = "full"
		res.Stats, err = ix.fullSync(ctx, ont, plan, q, source)
	} else {
		res.Mode = "incremental"
		res.Stats, err = ix.incremental(ctx, ont, plan, q, source, cur)
	}
	ix.recordRun(ctx, ont.APIName, source, res.Stats, err)
	if err != nil {
		res.Error = err.Error()
	}
	return res, err
}

func (ix *Indexer) incremental(ctx context.Context, ont *spec.CompiledOntology, p *sourcePlan, q Querier, source string, cur cursor) (objstore.SyncStats, error) {
	var total objstore.SyncStats
	for {
		rows, err := ReadChanges(ctx, q, p, cur.ts, cur.pk, ix.batch())
		if err != nil || len(rows) == 0 {
			return total, err
		}
		last := rows[len(rows)-1]
		next := cursor{ts: &last.TS, pk: last.PK}
		err = pgx.BeginFunc(ctx, ix.DB, func(tx pgx.Tx) error {
			st, err := ix.Store.UpsertFromSource(ctx, tx, ont, p.ot.APIName, rows)
			if err != nil {
				return err
			}
			total.Add(st)
			return saveCursor(ctx, tx, ont.APIName, source, next, false) // cursor moves with the data
		})
		if err != nil {
			return total, err
		}
		cur = next
		if len(rows) < ix.batch() {
			return total, nil
		}
	}
}

// fullSync reads every source row, merges it, and soft-deletes source-backed objects that are
// gone. The whole snapshot commits in one transaction so readers never see a half-applied view.
func (ix *Indexer) fullSync(ctx context.Context, ont *spec.CompiledOntology, p *sourcePlan, q Querier, source string) (objstore.SyncStats, error) {
	var total objstore.SyncStats
	var maxTS *time.Time
	var maxPK string
	present := []string{}
	err := pgx.BeginFunc(ctx, ix.DB, func(tx pgx.Tx) error {
		after := ""
		for {
			rows, err := ReadPage(ctx, q, p, after, ix.batch())
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			st, err := ix.Store.UpsertFromSource(ctx, tx, ont, p.ot.APIName, rows)
			if err != nil {
				return err
			}
			total.Add(st)
			for _, r := range rows {
				present = append(present, r.PK)
				if p.tsCol != "" && (maxTS == nil || r.TS.After(*maxTS) || (r.TS.Equal(*maxTS) && r.PK > maxPK)) {
					ts := r.TS
					maxTS, maxPK = &ts, r.PK
				}
			}
			after = rows[len(rows)-1].PK
			if len(rows) < ix.batch() {
				break
			}
		}
		existing, err := ix.Store.CountSourced(ctx, tx, ont, p.ot.APIName)
		if err != nil {
			return err
		}
		if gone := existing - (total.Inserted + total.Updated + total.Unchanged); !ix.Force && existing >= 10 &&
			float64(gone) > MaxDeleteFraction*float64(existing) {
			return fmt.Errorf("%w: %d of %d %s objects are missing from the source; rerun with force after checking it",
				ErrDeleteGuard, gone, existing, p.ot.APIName)
		}
		deleted, err := ix.Store.MarkMissingFromSource(ctx, tx, ont, p.ot.APIName, present)
		if err != nil {
			return err
		}
		total.Deleted = deleted
		return saveCursor(ctx, tx, ont.APIName, source, cursor{ts: maxTS, pk: maxPK}, true)
	})
	return total, err
}

// SyncLinkType syncs one many-to-many join table (always a full snapshot).
func (ix *Indexer) SyncLinkType(ctx context.Context, ont *spec.CompiledOntology, name string) (Result, error) {
	source := "link:" + name
	res := Result{Source: source, Mode: "full"}
	lt := ont.LinkTypes[name]
	if lt == nil || lt.JoinTable == nil {
		return res, fmt.Errorf("link type %q has no join table", name)
	}
	q, err := ix.source(lt.JoinTable.Connector)
	if err != nil {
		return res, err
	}
	from, to, err := ReadJoinPairs(ctx, q, lt.JoinTable)
	if err == nil {
		err = pgx.BeginFunc(ctx, ix.DB, func(tx pgx.Tx) error {
			st, err := ix.Store.SyncLinkPairs(ctx, tx, lt, from, to, true)
			res.Stats = st
			if err != nil {
				return err
			}
			return saveCursor(ctx, tx, ont.APIName, source, cursor{}, true)
		})
	}
	ix.recordRun(ctx, ont.APIName, source, res.Stats, err)
	if err != nil {
		res.Error = err.Error()
	}
	return res, err
}

// SyncAll syncs every datasource-backed object type, then every join table. A failing source is
// reported and does not stop the others.
func (ix *Indexer) SyncAll(ctx context.Context, ont *spec.CompiledOntology, forceFull bool) []Result {
	var out []Result
	for _, name := range sortedKeys(ont.ObjectTypes) {
		if ont.ObjectTypes[name].Datasource == nil {
			continue
		}
		r, err := ix.SyncObjectType(ctx, ont, name, forceFull)
		if err != nil {
			slog.ErrorContext(ctx, "indexer: object sync failed", "objectType", name, "err", err)
		}
		out = append(out, r)
	}
	for _, name := range sortedKeys(ont.LinkTypes) {
		if ont.LinkTypes[name].JoinTable == nil {
			continue
		}
		r, err := ix.SyncLinkType(ctx, ont, name)
		if err != nil {
			slog.ErrorContext(ctx, "indexer: link sync failed", "linkType", name, "err", err)
		}
		out = append(out, r)
	}
	return out
}

// Run syncs on every tick until ctx is cancelled. ontologyFn is called each round so a newly
// published version is picked up without a restart.
func (ix *Indexer) Run(ctx context.Context, interval time.Duration, ontologyFn func(context.Context) (*spec.CompiledOntology, error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if ont, err := ontologyFn(ctx); err != nil {
			slog.WarnContext(ctx, "indexer: no ontology", "err", err)
		} else {
			for _, r := range ix.SyncAll(ctx, ont, false) {
				if r.Error == "" && (r.Stats.Inserted+r.Stats.Updated+r.Stats.Deleted) > 0 {
					slog.InfoContext(ctx, "indexer: synced", "source", r.Source, "mode", r.Mode, "stats", r.Stats)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
