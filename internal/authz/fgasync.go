package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// DesiredTuples renders an object type's authz templates against one object. Deleted objects
// and templates referencing a null property yield no tuples.
func DesiredTuples(ot *spec.ObjectType, pk string, props map[string]any, deleted bool) []Tuple {
	if ot.Authz == nil || deleted {
		return nil
	}
	object := ot.Authz.FGAType + ":" + pk
	seen := map[Tuple]bool{}
	var out []Tuple
	for _, t := range ot.Authz.Tuples {
		user, ok := spec.RenderTuple(t.Target, props)
		if !ok || !strings.Contains(user, ":") {
			continue
		}
		tu := Tuple{User: user, Relation: t.Relation, Object: object}
		if !seen[tu] {
			seen[tu] = true
			out = append(out, tu)
		}
	}
	return out
}

// DiffTuples returns what to write and delete to go from have to want.
func DiffTuples(have, want []Tuple) (writes, deletes []Tuple) {
	h := map[Tuple]bool{}
	for _, t := range have {
		h[t] = true
	}
	w := map[Tuple]bool{}
	for _, t := range want {
		w[t] = true
		if !h[t] {
			writes = append(writes, t)
		}
	}
	for _, t := range have {
		if !w[t] {
			deletes = append(deletes, t)
		}
	}
	less := func(s []Tuple) {
		sort.Slice(s, func(i, j int) bool {
			return s[i].Object+s[i].Relation+s[i].User < s[j].Object+s[j].Relation+s[j].User
		})
	}
	less(writes)
	less(deletes)
	return writes, deletes
}

// FGASync keeps OpenFGA tuples in line with object data (Spec §8.3). authz.derived_tuple records
// what was written, so a removed or changed property deletes exactly the tuples it produced and
// tuples managed elsewhere (group memberships, manual grants) are never touched.
type FGASync struct {
	DB     *pgxpool.Pool
	Store  *objstore.Store
	Writer TupleWriter
}

// ErrNoAuthz means the object type has no authz block.
var ErrNoAuthz = errors.New("object type has no authz block")

func (s *FGASync) stored(ctx context.Context, typ string, pks []string) (map[string][]Tuple, error) {
	rows, err := s.DB.Query(ctx, `SELECT object_pk, fga_user, relation, fga_object FROM authz.derived_tuple
WHERE object_type = $1 AND object_pk = ANY($2::text[])`, typ, pks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Tuple{}
	for rows.Next() {
		var pk string
		var t Tuple
		if err := rows.Scan(&pk, &t.User, &t.Relation, &t.Object); err != nil {
			return nil, err
		}
		out[pk] = append(out[pk], t)
	}
	return out, rows.Err()
}

// SyncObjects reconciles the tuples of the given objects. Objects that no longer exist (or are
// soft-deleted) lose their derived tuples. OpenFGA is written before the record is updated, so
// a crash in between is repaired by the next run (writes and deletes are idempotent).
func (s *FGASync) SyncObjects(ctx context.Context, ont *spec.CompiledOntology, typ string, pks []string) (writes, deletes int, err error) {
	ot := ont.ObjectTypes[typ]
	if ot == nil {
		return 0, 0, fmt.Errorf("unknown object type %q", typ)
	}
	if ot.Authz == nil {
		return 0, 0, ErrNoAuthz
	}
	if len(pks) == 0 {
		return 0, 0, nil
	}
	refs := make([]objstore.ObjectRef, len(pks))
	for i, pk := range pks {
		refs[i] = objstore.ObjectRef{Type: typ, PK: pk}
	}
	live, err := s.Store.GetMany(ctx, ont, refs)
	if err != nil {
		return 0, 0, err
	}
	have, err := s.stored(ctx, typ, pks)
	if err != nil {
		return 0, 0, err
	}
	var allW, allD []Tuple
	want := map[string][]Tuple{}
	for _, pk := range pks {
		o := live[objstore.ObjectRef{Type: typ, PK: pk}]
		if o != nil {
			want[pk] = DesiredTuples(ot, pk, o.Props, false)
		}
		w, d := DiffTuples(have[pk], want[pk])
		allW, allD = append(allW, w...), append(allD, d...)
	}
	if len(allW) == 0 && len(allD) == 0 {
		return 0, 0, nil
	}
	if err := s.Writer.WriteTuples(ctx, allW, allD); err != nil {
		return 0, 0, err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM authz.derived_tuple WHERE object_type = $1 AND object_pk = ANY($2::text[])`, typ, pks); err != nil {
			return err
		}
		for pk, ts := range want {
			for _, t := range ts {
				if _, err := tx.Exec(ctx, `INSERT INTO authz.derived_tuple (object_type, object_pk, fga_user, relation, fga_object)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, typ, pk, t.User, t.Relation, t.Object); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return len(allW), len(allD), err
}

// FullSync reconciles every object of an authz-bound type (initial load, or `ontoctl fga sync`).
func (s *FGASync) FullSync(ctx context.Context, ont *spec.CompiledOntology, typ string, batch int) (writes, deletes int, err error) {
	ot := ont.ObjectTypes[typ]
	if ot == nil {
		return 0, 0, fmt.Errorf("unknown object type %q", typ)
	}
	if ot.Authz == nil {
		return 0, 0, ErrNoAuthz
	}
	if batch <= 0 {
		batch = 500
	}
	table := ot.TableName
	if !strings.Contains(table, ".") {
		table = "obj." + table
	}
	schema, name, _ := strings.Cut(table, ".")
	q := fmt.Sprintf(`SELECT pk FROM (
  SELECT "_pk" AS pk FROM %s
  UNION SELECT object_pk FROM authz.derived_tuple WHERE object_type = $1
) all_pks WHERE pk > $2 ORDER BY pk LIMIT %d`, pgx.Identifier{schema, name}.Sanitize(), batch)
	after := ""
	for {
		rows, err := s.DB.Query(ctx, q, typ, after)
		if err != nil {
			return writes, deletes, err
		}
		pks, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return writes, deletes, err
		}
		if len(pks) == 0 {
			return writes, deletes, nil
		}
		w, d, err := s.SyncObjects(ctx, ont, typ, pks)
		writes, deletes = writes+w, deletes+d
		if err != nil {
			return writes, deletes, err
		}
		after = pks[len(pks)-1]
		if len(pks) < batch {
			return writes, deletes, nil
		}
	}
}

// ProcessOutbox reconciles objects named by new ObjectEdited outbox events (written by the
// action service). Progress is stored in authz.sync_cursor under name.
func (s *FGASync) ProcessOutbox(ctx context.Context, ont *spec.CompiledOntology, name string, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	var last int64
	err := s.DB.QueryRow(ctx, `SELECT last_id FROM authz.sync_cursor WHERE name = $1`, name).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id, aggregate, aggregate_id FROM edits.outbox
WHERE id > $1 AND event_type = 'ObjectEdited' ORDER BY id LIMIT $2`, last, limit)
	if err != nil {
		return 0, err
	}
	byType := map[string][]string{}
	seen := map[string]bool{}
	maxID := last
	n := 0
	for rows.Next() {
		var id int64
		var typ, pk string
		if err := rows.Scan(&id, &typ, &pk); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		if id > maxID {
			maxID = id
		}
		if k := typ + "\x00" + pk; !seen[k] {
			seen[k] = true
			byType[typ] = append(byType[typ], pk)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	for _, typ := range sortedKeys(byType) {
		ot := ont.ObjectTypes[typ]
		if ot == nil || ot.Authz == nil {
			continue
		}
		if _, _, err := s.SyncObjects(ctx, ont, typ, byType[typ]); err != nil {
			return 0, fmt.Errorf("sync %s: %w", typ, err) // cursor not advanced: retried next round
		}
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO authz.sync_cursor (name, last_id) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET last_id = EXCLUDED.last_id, updated_at = now()`, name, maxID)
	return n, err
}

// Run processes the outbox continuously and does a full sync every fullInterval (which also
// covers objects changed by the indexer, since it writes object tables without outbox events).
func (s *FGASync) Run(ctx context.Context, interval, fullInterval time.Duration, ontologyFn func(context.Context) (*spec.CompiledOntology, error)) {
	var lastFull time.Time
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if ont, err := ontologyFn(ctx); err != nil {
			slog.WarnContext(ctx, "fga sync: no ontology", "err", err)
		} else {
			if time.Since(lastFull) >= fullInterval {
				for _, typ := range sortedKeys(ont.ObjectTypes) {
					if ont.ObjectTypes[typ].Authz == nil {
						continue
					}
					w, d, err := s.FullSync(ctx, ont, typ, 0)
					if err != nil {
						slog.ErrorContext(ctx, "fga sync: full sync failed", "objectType", typ, "err", err)
						continue
					}
					if w+d > 0 {
						slog.InfoContext(ctx, "fga sync: reconciled", "objectType", typ, "writes", w, "deletes", d)
					}
				}
				lastFull = time.Now()
			}
			for {
				n, err := s.ProcessOutbox(ctx, ont, "fga", 1000)
				if err != nil {
					slog.ErrorContext(ctx, "fga sync: outbox", "err", err)
				}
				if err != nil || n < 1000 {
					break
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
