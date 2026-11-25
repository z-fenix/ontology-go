// Package indexer syncs datasource rows into the materialized object tables (Spec §5).
//
// Two modes share one merge path (objstore.UpsertFromSource):
//   - incremental: rows whose timestamp column passed the stored cursor are read in pages.
//   - full snapshot: every row is read; objects missing from the source are soft-deleted.
//     Object types without a timestampColumn are always synced this way.
//
// Debezium change events (debezium.go) map onto the same merge, so a Kafka consumer can be
// added without touching the merge or cursor logic.
package indexer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// Querier is the read side of a source database (satisfied by *pgxpool.Pool).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func tableIdent(name string) string {
	if s, t, ok := strings.Cut(name, "."); ok {
		return ident(s, t)
	}
	return ident(name)
}

// column maps one property to its source column.
type column struct {
	prop string
	col  string
	base spec.BaseType
}

// sourcePlan is what the indexer reads for one object type.
type sourcePlan struct {
	ot      *spec.ObjectType
	table   string
	pkCol   string
	tsCol   string
	columns []column
}

func planFor(ot *spec.ObjectType) (*sourcePlan, error) {
	ds := ot.Datasource
	if ds == nil {
		return nil, fmt.Errorf("object type %s has no datasource", ot.APIName)
	}
	p := &sourcePlan{ot: ot, table: ds.Table, tsCol: ds.TimestampColumn}
	for _, name := range ot.PropertyOrder {
		prop := ot.Properties[name]
		if prop.Type.Base == spec.TVector || prop.DerivedFrom != "" {
			continue // derived values are computed, not read from the source
		}
		p.columns = append(p.columns, column{prop: name, col: prop.Column, base: prop.Type.Base})
		if name == ot.PrimaryKey {
			p.pkCol = prop.Column
		}
	}
	if p.pkCol == "" {
		return nil, fmt.Errorf("object type %s: primary key %q has no source column", ot.APIName, ot.PrimaryKey)
	}
	return p, nil
}

// selectList casts numeric and date-like columns to text so values reach objstore in the
// representation its encoder expects, independent of pgx's type mapping.
func (p *sourcePlan) selectList() string {
	parts := make([]string, 0, len(p.columns)+1)
	for _, c := range p.columns {
		expr := ident(c.col)
		switch c.base {
		case spec.TDecimal, spec.TDate:
			expr += "::text"
		case spec.TInteger, spec.TLong:
			expr += "::bigint"
		case spec.TDouble:
			expr += "::double precision"
		case spec.TGeoPoint, spec.TArray, spec.TStruct:
			expr = "to_jsonb(" + expr + ")::text"
		}
		parts = append(parts, expr)
	}
	if p.tsCol != "" {
		parts = append(parts, ident(p.tsCol))
	}
	return strings.Join(parts, ", ")
}

func (p *sourcePlan) scan(rows pgx.Rows) (objstore.SourceRow, error) {
	vals := make([]any, len(p.columns))
	dest := make([]any, 0, len(p.columns)+1)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	var ts *time.Time
	if p.tsCol != "" {
		dest = append(dest, &ts)
	}
	if err := rows.Scan(dest...); err != nil {
		return objstore.SourceRow{}, err
	}
	r := objstore.SourceRow{Props: make(map[string]any, len(p.columns)), TS: time.Now().UTC()}
	if ts != nil {
		r.TS = ts.UTC()
	}
	for i, c := range p.columns {
		v, err := normalize(c.base, vals[i])
		if err != nil {
			return r, fmt.Errorf("column %s: %w", c.col, err)
		}
		r.Props[c.prop] = v
		if c.prop == p.ot.PrimaryKey {
			r.PK = fmt.Sprint(v)
		}
	}
	return r, nil
}

// ReadChanges reads rows changed after (afterTS, afterPK) in timestamp order, at most limit.
func ReadChanges(ctx context.Context, q Querier, p *sourcePlan, afterTS *time.Time, afterPK string, limit int) ([]objstore.SourceRow, error) {
	ts, pk := ident(p.tsCol), ident(p.pkCol)
	where, args := "TRUE", []any{}
	if afterTS != nil {
		where = fmt.Sprintf("(%s > $1 OR (%s = $1 AND %s::text > $2))", ts, ts, pk)
		args = append(args, *afterTS, afterPK)
	}
	sql := fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL AND %s ORDER BY %s, %s::text LIMIT %d`,
		p.selectList(), tableIdent(p.table), ts, where, ts, pk, limit)
	return collect(ctx, q, p, sql, args...)
}

// ReadPage reads one page of a full snapshot in primary-key order.
func ReadPage(ctx context.Context, q Querier, p *sourcePlan, afterPK string, limit int) ([]objstore.SourceRow, error) {
	pk := ident(p.pkCol)
	sql := fmt.Sprintf(`SELECT %s FROM %s WHERE %s::text > $1 ORDER BY %s::text LIMIT %d`,
		p.selectList(), tableIdent(p.table), pk, pk, limit)
	return collect(ctx, q, p, sql, afterPK)
}

func collect(ctx context.Context, q Querier, p *sourcePlan, sql string, args ...any) ([]objstore.SourceRow, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.table, err)
	}
	defer rows.Close()
	var out []objstore.SourceRow
	for rows.Next() {
		r, err := p.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p.table, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadJoinPairs reads every (from, to) pair of a many-to-many join table.
func ReadJoinPairs(ctx context.Context, q Querier, jt *spec.JoinTable) ([]string, []string, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s::text, %s::text FROM %s WHERE %s IS NOT NULL AND %s IS NOT NULL`,
		ident(jt.FromColumn), ident(jt.ToColumn), tableIdent(jt.Table), ident(jt.FromColumn), ident(jt.ToColumn)))
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", jt.Table, err)
	}
	defer rows.Close()
	var from, to []string
	for rows.Next() {
		var f, t string
		if err := rows.Scan(&f, &t); err != nil {
			return nil, nil, err
		}
		from, to = append(from, f), append(to, t)
	}
	return from, to, rows.Err()
}
