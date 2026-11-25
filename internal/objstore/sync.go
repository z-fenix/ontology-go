package objstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acme/ontology/internal/spec"
)

// UserDeletedMarker is recorded in _edited_props when an action deletes an object, so a source
// row that still contains the object does not bring it back.
const UserDeletedMarker = "_deleted"

// SourceRow is one row read from an object type's datasource, keyed by property apiName.
type SourceRow struct {
	PK    string
	Props map[string]any
	TS    time.Time // value of the datasource timestampColumn, or the read time when there is none
}

// SyncStats counts what a sync did.
type SyncStats struct {
	Inserted  int `json:"inserted"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`
	Deleted   int `json:"deleted"`
}

// Add accumulates b into s.
func (s *SyncStats) Add(b SyncStats) {
	s.Inserted += b.Inserted
	s.Updated += b.Updated
	s.Unchanged += b.Unchanged
	s.Skipped += b.Skipped
	s.Deleted += b.Deleted
}

type currentState struct {
	edited  map[string]bool
	editTS  *time.Time
	deleted bool
}

type mergePlan struct {
	write map[string]any // properties the source may write
	clear []string       // edited markers to drop because the source value won
	skip  bool           // the row must not touch the object at all
}

// planMerge applies the object type's conflict strategy (Spec §5.2):
//
//	APPLY_USER_EDITS  — an edited property keeps the user's value; the source updates the rest.
//	MOST_RECENT_VALUE — when the source row is newer than the object's last edit, the source
//	                    overwrites edited properties too (and they stop counting as edited).
//
// An object deleted by a user stays deleted under both strategies. Edit times are tracked per
// object (_edit_ts), not per property, so MOST_RECENT_VALUE compares against the latest edit.
func planMerge(strategy spec.ConflictStrategy, row SourceRow, cur *currentState) mergePlan {
	if cur == nil {
		return mergePlan{write: row.Props}
	}
	if cur.deleted && cur.edited[UserDeletedMarker] {
		return mergePlan{skip: true}
	}
	sourceNewer := strategy == spec.MostRecentValue && cur.editTS != nil && row.TS.After(*cur.editTS)
	p := mergePlan{write: map[string]any{}}
	for k, v := range row.Props {
		if cur.edited[k] && !sourceNewer {
			continue
		}
		p.write[k] = v
		if cur.edited[k] {
			p.clear = append(p.clear, k)
		}
	}
	sort.Strings(p.clear)
	return p
}

func sourceMeta(ont *spec.CompiledOntology, typ string) (*tableMeta, error) {
	m, err := meta(ont, typ)
	if err != nil {
		return nil, err
	}
	if m.ot.Datasource == nil {
		return nil, fmt.Errorf("%w: object type %s has no datasource", ErrUnknownType, typ)
	}
	return m, nil
}

func loadState(ctx context.Context, tx DB, m *tableMeta, pks []string) (map[string]*currentState, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT "_pk", "_edited_props", "_edit_ts", "_deleted" FROM %s WHERE "_pk" = ANY($1)`, m.ident), pks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*currentState{}
	for rows.Next() {
		var pk string
		var edited []string
		st := &currentState{edited: map[string]bool{}}
		if err := rows.Scan(&pk, &edited, &st.editTS, &st.deleted); err != nil {
			return nil, err
		}
		for _, e := range edited {
			st.edited[e] = true
		}
		out[pk] = st
	}
	return out, rows.Err()
}

// storedProps keeps the properties materialized in PostgreSQL and sets the primary key.
func storedProps(m *tableMeta, row SourceRow) (map[string]any, error) {
	out := make(map[string]any, len(row.Props)+1)
	for k, v := range row.Props {
		if _, ok := m.byProp[k]; ok {
			out[k] = v
		}
	}
	pk, err := pkToValue(m.ot.Properties[m.ot.PrimaryKey], row.PK)
	if err != nil {
		return nil, err
	}
	out[m.ot.PrimaryKey] = pk
	return out, nil
}

// UpsertFromSource merges datasource rows into the object table inside tx. A changed object gets
// a new _version, so actions that read it earlier fail with STALE_OBJECT and re-read; an object
// whose values did not change keeps its version.
func (s *Store) UpsertFromSource(ctx context.Context, tx DB, ont *spec.CompiledOntology, typ string, rows []SourceRow) (SyncStats, error) {
	var st SyncStats
	m, err := sourceMeta(ont, typ)
	if err != nil {
		return st, err
	}
	strategy := m.ot.Datasource.ConflictStrategy
	pks := make([]string, 0, len(rows))
	for _, r := range rows {
		pks = append(pks, r.PK)
	}
	cur, err := loadState(ctx, tx, m, pks)
	if err != nil {
		return st, err
	}
	for _, r := range rows {
		if r.PK == "" {
			st.Skipped++
			continue
		}
		props, err := storedProps(m, r)
		if err != nil {
			return st, err
		}
		row := SourceRow{PK: r.PK, Props: props, TS: r.TS}
		state := cur[r.PK]
		if state == nil {
			ok, err := insertSource(ctx, tx, m, row)
			if err != nil {
				return st, fmt.Errorf("%s/%s: %w", typ, r.PK, err)
			}
			if ok {
				st.Inserted++
			} else {
				st.Skipped++ // created concurrently (e.g. by an action); merged on the next run
			}
			cur[r.PK] = &currentState{edited: map[string]bool{}}
			continue
		}
		plan := planMerge(strategy, row, state)
		if plan.skip {
			st.Skipped++
			continue
		}
		changed, err := updateSource(ctx, tx, m, row, plan, strategy == spec.MostRecentValue)
		if err != nil {
			return st, fmt.Errorf("%s/%s: %w", typ, r.PK, err)
		}
		if changed {
			st.Updated++
			state.deleted = false
		} else {
			st.Unchanged++
		}
	}
	return st, nil
}

func insertSource(ctx context.Context, tx DB, m *tableMeta, row SourceRow) (bool, error) {
	cols := []string{`"_pk"`, `"_rid"`, `"_source_ts"`}
	vals := []string{"$1", "$2", "$3"}
	args := []any{row.PK, newRID(), row.TS}
	for _, name := range propNames(row.Props) {
		c := m.byProp[name]
		v, err := encodeValue(c.prop, row.Props[name])
		if err != nil {
			return false, err
		}
		args = append(args, v)
		cols = append(cols, ident(c.name))
		vals = append(vals, placeholder(c.prop, len(args)))
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT ("_pk") DO NOTHING RETURNING "_version"`,
		m.ident, strings.Join(cols, ", "), strings.Join(vals, ", "))
	var ver int64
	err := tx.QueryRow(ctx, q, args...).Scan(&ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// updateSource writes the plan only when a value actually differs (compared in SQL, so numeric
// scale and JSON key order never cause spurious version bumps), or to undo a source deletion.
func updateSource(ctx context.Context, tx DB, m *tableMeta, row SourceRow, plan mergePlan, trackTS bool) (bool, error) {
	clear := plan.clear
	if clear == nil {
		clear = []string{}
	}
	args := []any{row.PK, row.TS, clear}
	var sets, diffs []string
	for _, name := range propNames(plan.write) {
		if name == m.ot.PrimaryKey {
			continue
		}
		c := m.byProp[name]
		v, err := encodeValue(c.prop, plan.write[name])
		if err != nil {
			return false, err
		}
		args = append(args, v)
		ph := placeholder(c.prop, len(args))
		sets = append(sets, ident(c.name)+" = "+ph)
		diffs = append(diffs, ident(c.name)+" IS DISTINCT FROM "+ph)
	}
	diffs = append(diffs, `"_deleted"`, `cardinality($3::text[]) > 0`)
	sets = append(sets,
		`"_source_ts" = $2`,
		`"_deleted" = false`,
		`"_version" = "_version" + 1`,
		`"_updated_at" = now()`,
		`"_edited_props" = ARRAY(SELECT p FROM unnest("_edited_props") AS p WHERE NOT (p = ANY($3::text[])) ORDER BY p)`,
	)
	q := fmt.Sprintf(`UPDATE %s SET %s WHERE "_pk" = $1 AND (%s)`, m.ident, strings.Join(sets, ", "), strings.Join(diffs, " OR "))
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	if trackTS { // keep the source timestamp current without bumping the version
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET "_source_ts" = $2 WHERE "_pk" = $1 AND "_source_ts" IS DISTINCT FROM $2`, m.ident), row.PK, row.TS); err != nil {
			return false, err
		}
	}
	return false, nil
}

const deleteFromSource = `UPDATE %s SET "_deleted" = true, "_version" = "_version" + 1, "_updated_at" = now()
WHERE "_source_ts" IS NOT NULL AND NOT "_deleted" AND %s`

// DeleteFromSource soft-deletes objects whose source rows were deleted. Objects created only by
// actions (no source timestamp) are never touched.
func (s *Store) DeleteFromSource(ctx context.Context, tx DB, ont *spec.CompiledOntology, typ string, pks []string) (int, error) {
	m, err := sourceMeta(ont, typ)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(deleteFromSource, m.ident, `"_pk" = ANY($1::text[])`), pks)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// MarkMissingFromSource soft-deletes source-backed objects absent from a full snapshot.
func (s *Store) MarkMissingFromSource(ctx context.Context, tx DB, ont *spec.CompiledOntology, typ string, present []string) (int, error) {
	m, err := sourceMeta(ont, typ)
	if err != nil {
		return 0, err
	}
	if present == nil {
		present = []string{}
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(deleteFromSource, m.ident, `NOT ("_pk" = ANY($1::text[]))`), present)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// CountSourced returns how many live objects of typ came from the datasource.
func (s *Store) CountSourced(ctx context.Context, db DB, ont *spec.CompiledOntology, typ string) (int, error) {
	m, err := sourceMeta(ont, typ)
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE "_source_ts" IS NOT NULL AND NOT "_deleted"`, m.ident)).Scan(&n)
	return n, err
}

// SyncLinkPairs merges a many-to-many link's join-table rows. Rows changed by actions
// (_source = 'EDIT', including user unlinks) are never overwritten. With full=true, datasource
// rows absent from pairs are soft-deleted.
func (s *Store) SyncLinkPairs(ctx context.Context, tx DB, lt *spec.LinkType, from, to []string, full bool) (SyncStats, error) {
	var st SyncStats
	if !isManyToMany(lt) {
		return st, fmt.Errorf("%w: link %s is not many-to-many", ErrUnknownType, lt.APIName)
	}
	if len(from) != len(to) {
		return st, fmt.Errorf("%w: from/to length mismatch", ErrInvalidEdit)
	}
	seen := map[[2]string]bool{}
	f, t := []string{}, []string{}
	for i := range from {
		k := [2]string{from[i], to[i]}
		if from[i] == "" || to[i] == "" || seen[k] {
			continue
		}
		seen[k] = true
		f, t = append(f, from[i]), append(t, to[i])
	}
	table := tableIdent(LinkTableName(lt))
	tag, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s AS l ("from_pk", "to_pk", "_source", "_deleted")
SELECT s.f, s.t, 'DATASOURCE', false FROM unnest($1::text[], $2::text[]) AS s(f, t)
ON CONFLICT ("from_pk", "to_pk") DO UPDATE SET "_deleted" = false, "_updated_at" = now()
WHERE l."_source" = 'DATASOURCE' AND l."_deleted"`, table), f, t)
	if err != nil {
		return st, err
	}
	st.Inserted = int(tag.RowsAffected())
	if full {
		tag, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s AS l SET "_deleted" = true, "_updated_at" = now()
WHERE l."_source" = 'DATASOURCE' AND NOT l."_deleted"
  AND NOT EXISTS (SELECT 1 FROM unnest($1::text[], $2::text[]) AS s(f, t) WHERE s.f = l."from_pk" AND s.t = l."to_pk")`, table), f, t)
		if err != nil {
			return st, err
		}
		st.Deleted = int(tag.RowsAffected())
	}
	return st, nil
}
