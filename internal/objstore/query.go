package objstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/acme/ontology/internal/spec"
)

// ErrInvalidQuery is returned for malformed filters, unknown fields and unsupported operators.
var ErrInvalidQuery = errors.New("invalid query")

// Query limits.
const (
	MaxLinkDepth    = 3
	MaxSearchLimit  = 10000
	DefaultAggLimit = 1000
	MaxAggLimit     = 10000
)

// Cmp compares a property with a value.
type Cmp struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}

// InCmp matches a property against a list of values.
type InCmp struct {
	Field  string `json:"field"`
	Values []any  `json:"values"`
}

// FieldRef names a property.
type FieldRef struct {
	Field string `json:"field"`
}

// LinkCmp matches objects with at least one linked object satisfying Where.
type LinkCmp struct {
	Path  string `json:"path"`
	Where *Where `json:"where,omitempty"`
}

// Where is the object-set filter DSL (Spec §8.3). Exactly one operator must be set.
type Where struct {
	And        []Where   `json:"and,omitempty"`
	Or         []Where   `json:"or,omitempty"`
	Not        *Where    `json:"not,omitempty"`
	Eq         *Cmp      `json:"eq,omitempty"`
	Gt         *Cmp      `json:"gt,omitempty"`
	Gte        *Cmp      `json:"gte,omitempty"`
	Lt         *Cmp      `json:"lt,omitempty"`
	Lte        *Cmp      `json:"lte,omitempty"`
	In         *InCmp    `json:"in,omitempty"`
	Contains   *Cmp      `json:"contains,omitempty"`
	StartsWith *Cmp      `json:"startsWith,omitempty"`
	IsNull     *FieldRef `json:"isNull,omitempty"`
	Link       *LinkCmp  `json:"link,omitempty"`
}

func (w *Where) op() (string, error) {
	var ops []string
	add := func(set bool, name string) {
		if set {
			ops = append(ops, name)
		}
	}
	add(w.And != nil, "and")
	add(w.Or != nil, "or")
	add(w.Not != nil, "not")
	add(w.Eq != nil, "eq")
	add(w.Gt != nil, "gt")
	add(w.Gte != nil, "gte")
	add(w.Lt != nil, "lt")
	add(w.Lte != nil, "lte")
	add(w.In != nil, "in")
	add(w.Contains != nil, "contains")
	add(w.StartsWith != nil, "startsWith")
	add(w.IsNull != nil, "isNull")
	add(w.Link != nil, "link")
	if len(ops) != 1 {
		return "", fmt.Errorf("%w: each condition needs exactly one operator, got %v", ErrInvalidQuery, ops)
	}
	return ops[0], nil
}

// QueryField is a property referenced by a query (used for field-level permission checks).
type QueryField struct {
	Type  string
	Field string
}

type builder struct {
	ont    *spec.CompiledOntology
	args   []any
	n      int
	fields []QueryField
}

func (b *builder) alias() string {
	b.n++
	return fmt.Sprintf("t%d", b.n)
}

func (b *builder) column(m *tableMeta, field string) (column, error) {
	c, ok := m.byProp[field]
	if !ok {
		if _, exists := m.ot.Properties[field]; exists {
			return column{}, fmt.Errorf("%w: property %s.%s cannot be queried", ErrInvalidQuery, m.ot.APIName, field)
		}
		return column{}, fmt.Errorf("%w: unknown property %s.%s", ErrInvalidQuery, m.ot.APIName, field)
	}
	b.fields = append(b.fields, QueryField{Type: m.ot.APIName, Field: field})
	return c, nil
}

func (b *builder) bind(p *spec.Property, v any) (string, error) {
	enc, err := encodeValue(p, v)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	b.args = append(b.args, enc)
	return placeholder(p, len(b.args)), nil
}

func (b *builder) raw(v any, cast string) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d%s", len(b.args), cast)
}

func orderable(t spec.BaseType) bool {
	switch t {
	case spec.TString, spec.TInteger, spec.TLong, spec.TDouble, spec.TDecimal, spec.TDate, spec.TTimestamp:
		return true
	}
	return false
}

func numeric(t spec.BaseType) bool {
	switch t {
	case spec.TInteger, spec.TLong, spec.TDouble, spec.TDecimal:
		return true
	}
	return false
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (b *builder) cond(m *tableMeta, alias string, w *Where, depth int) (string, error) {
	if w == nil {
		return "TRUE", nil
	}
	op, err := w.op()
	if err != nil {
		return "", err
	}
	switch op {
	case "and", "or":
		list, joiner, empty := w.And, " AND ", "TRUE"
		if op == "or" {
			list, joiner, empty = w.Or, " OR ", "FALSE"
		}
		if len(list) == 0 {
			return empty, nil
		}
		parts := make([]string, 0, len(list))
		for i := range list {
			s, err := b.cond(m, alias, &list[i], depth)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+s+")")
		}
		return strings.Join(parts, joiner), nil
	case "not":
		s, err := b.cond(m, alias, w.Not, depth)
		if err != nil {
			return "", err
		}
		return "NOT COALESCE((" + s + "), FALSE)", nil // NULL comparisons count as "not matching"
	case "isNull":
		c, err := b.column(m, w.IsNull.Field)
		if err != nil {
			return "", err
		}
		return ident(alias, c.name) + " IS NULL", nil
	case "in":
		c, err := b.column(m, w.In.Field)
		if err != nil {
			return "", err
		}
		if len(w.In.Values) == 0 {
			return "FALSE", nil
		}
		phs := make([]string, 0, len(w.In.Values))
		for _, v := range w.In.Values {
			if v == nil {
				return "", fmt.Errorf("%w: in() values must not be null (use isNull)", ErrInvalidQuery)
			}
			ph, err := b.bind(c.prop, v)
			if err != nil {
				return "", err
			}
			phs = append(phs, ph)
		}
		return ident(alias, c.name) + " IN (" + strings.Join(phs, ", ") + ")", nil
	case "link":
		return b.link(m, alias, w.Link, depth)
	}

	var cmp *Cmp
	sqlOp := ""
	switch op {
	case "eq":
		cmp, sqlOp = w.Eq, "="
	case "gt":
		cmp, sqlOp = w.Gt, ">"
	case "gte":
		cmp, sqlOp = w.Gte, ">="
	case "lt":
		cmp, sqlOp = w.Lt, "<"
	case "lte":
		cmp, sqlOp = w.Lte, "<="
	case "contains":
		cmp = w.Contains
	case "startsWith":
		cmp = w.StartsWith
	}
	c, err := b.column(m, cmp.Field)
	if err != nil {
		return "", err
	}
	col := ident(alias, c.name)
	base := c.prop.Type.Base
	switch op {
	case "eq":
		if cmp.Value == nil {
			return col + " IS NULL", nil
		}
		ph, err := b.bind(c.prop, cmp.Value)
		if err != nil {
			return "", err
		}
		return col + " = " + ph, nil
	case "contains":
		switch base {
		case spec.TString:
			s, ok := cmp.Value.(string)
			if !ok {
				return "", fmt.Errorf("%w: contains on %s needs a string", ErrInvalidQuery, cmp.Field)
			}
			return col + " ILIKE " + b.raw("%"+likeEscaper.Replace(s)+"%", "") + ` ESCAPE '\'`, nil
		case spec.TArray:
			if cmp.Value == nil {
				return "", fmt.Errorf("%w: contains needs a value", ErrInvalidQuery)
			}
			enc, err := json.Marshal([]any{cmp.Value})
			if err != nil {
				return "", fmt.Errorf("%w: %v", ErrInvalidQuery, err)
			}
			return col + " @> " + b.raw(string(enc), "::text::jsonb"), nil
		}
		return "", fmt.Errorf("%w: contains is only supported on string and array properties", ErrInvalidQuery)
	case "startsWith":
		s, ok := cmp.Value.(string)
		if base != spec.TString || !ok {
			return "", fmt.Errorf("%w: startsWith needs a string property and a string value", ErrInvalidQuery)
		}
		return col + " LIKE " + b.raw(likeEscaper.Replace(s)+"%", "") + ` ESCAPE '\'`, nil
	}
	if !orderable(base) {
		return "", fmt.Errorf("%w: %s is not supported on %s (%s)", ErrInvalidQuery, op, cmp.Field, c.prop.Type)
	}
	if cmp.Value == nil {
		return "", fmt.Errorf("%w: %s needs a value", ErrInvalidQuery, op)
	}
	ph, err := b.bind(c.prop, cmp.Value)
	if err != nil {
		return "", err
	}
	return col + " " + sqlOp + " " + ph, nil
}

func (b *builder) link(m *tableMeta, alias string, l *LinkCmp, depth int) (string, error) {
	if depth >= MaxLinkDepth {
		return "", fmt.Errorf("%w: link filters may be nested at most %d deep", ErrInvalidQuery, MaxLinkDepth)
	}
	rl, err := resolveLink(b.ont, m.ot.APIName, l.Path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	tm, err := meta(b.ont, rl.target)
	if err != nil {
		return "", err
	}
	t := b.alias()
	sub, err := b.cond(tm, t, l.Where, depth+1)
	if err != nil {
		return "", err
	}
	live := "NOT " + ident(t, "_deleted")
	if isManyToMany(rl.lt) {
		self, other := "from_pk", "to_pk"
		if !rl.fromSide {
			self, other = other, self
		}
		j := b.alias()
		return fmt.Sprintf("EXISTS (SELECT 1 FROM %s %s JOIN %s %s ON %s = %s WHERE %s = %s AND NOT %s AND %s AND (%s))",
			tableIdent(LinkTableName(rl.lt)), ident(j), tm.ident, ident(t), ident(t, "_pk"), ident(j, other),
			ident(j, self), ident(alias, "_pk"), ident(j, "_deleted"), live, sub), nil
	}
	fk := rl.lt.ForeignKey
	if rl.fromSide == fkHolderIsFrom(rl.lt) { // this object holds the foreign key
		fc, ok := m.byProp[fk.Property]
		if !ok {
			return "", fmt.Errorf("%w: foreign key %s.%s is not stored", ErrInvalidQuery, m.ot.APIName, fk.Property)
		}
		return fmt.Sprintf("EXISTS (SELECT 1 FROM %s %s WHERE %s = %s::text AND %s AND (%s))",
			tm.ident, ident(t), ident(t, "_pk"), ident(alias, fc.name), live, sub), nil
	}
	fc, ok := tm.byProp[fk.Property]
	if !ok {
		return "", fmt.Errorf("%w: foreign key %s.%s is not stored", ErrInvalidQuery, tm.ot.APIName, fk.Property)
	}
	return fmt.Sprintf("EXISTS (SELECT 1 FROM %s %s WHERE %s::text = %s AND %s AND (%s))",
		tm.ident, ident(t), ident(t, fc.name), ident(alias, "_pk"), live, sub), nil
}

// OrderBy sorts search results.
type OrderBy struct {
	Field     string `json:"field"`
	Direction string `json:"direction,omitempty"` // asc (default) | desc
}

func (b *builder) order(m *tableMeta, alias string, ob []OrderBy) (string, error) {
	parts := make([]string, 0, len(ob)+1)
	for _, o := range ob {
		c, err := b.column(m, o.Field)
		if err != nil {
			return "", err
		}
		if !orderable(c.prop.Type.Base) && c.prop.Type.Base != spec.TBoolean {
			return "", fmt.Errorf("%w: cannot sort by %s (%s)", ErrInvalidQuery, o.Field, c.prop.Type)
		}
		dir := strings.ToUpper(o.Direction)
		if dir == "" {
			dir = "ASC"
		}
		if dir != "ASC" && dir != "DESC" {
			return "", fmt.Errorf("%w: direction must be asc or desc", ErrInvalidQuery)
		}
		parts = append(parts, ident(alias, c.name)+" "+dir+" NULLS LAST")
	}
	parts = append(parts, ident(alias, "_pk")+" ASC") // deterministic pagination
	return strings.Join(parts, ", "), nil
}

// QueryFields validates a filter (plus extra root-type fields) and lists every property it reads,
// including properties of linked object types.
func QueryFields(ont *spec.CompiledOntology, typ string, w *Where, extra []string) ([]QueryField, error) {
	m, err := meta(ont, typ)
	if err != nil {
		return nil, err
	}
	b := &builder{ont: ont}
	if _, err := b.cond(m, "t0", w, 0); err != nil {
		return nil, err
	}
	for _, f := range extra {
		if _, err := b.column(m, f); err != nil {
			return nil, err
		}
	}
	return b.fields, nil
}

// SearchQuery is one page of an object-set search.
type SearchQuery struct {
	Where   *Where
	OrderBy []OrderBy
	Offset  int
	Limit   int
}

// Search returns live objects matching q and whether more rows follow.
func (s *Store) Search(ctx context.Context, ont *spec.CompiledOntology, typ string, q SearchQuery) ([]*Object, bool, error) {
	m, err := meta(ont, typ)
	if err != nil {
		return nil, false, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	if q.Offset < 0 {
		return nil, false, fmt.Errorf("%w: negative offset", ErrInvalidQuery)
	}
	b := &builder{ont: ont}
	where, err := b.cond(m, "t0", q.Where, 0)
	if err != nil {
		return nil, false, err
	}
	order, err := b.order(m, "t0", q.OrderBy)
	if err != nil {
		return nil, false, err
	}
	sql := fmt.Sprintf(`SELECT %s FROM %s %s WHERE NOT %s AND (%s) ORDER BY %s LIMIT %d OFFSET %d`,
		m.selectList(), m.ident, ident("t0"), ident("t0", "_deleted"), where, order, limit+1, q.Offset)
	rows, err := s.db.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, false, fmt.Errorf("search %s: %w", typ, err)
	}
	defer rows.Close()
	var out []*Object
	for rows.Next() {
		o, err := m.scan(rows.Scan)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// Metric is one aggregate (count, sum, avg, min, max).
type Metric struct {
	Op    string `json:"op"`
	Field string `json:"field,omitempty"`
	Name  string `json:"name,omitempty"`
}

// MetricName is the key a metric's value is reported under.
func MetricName(mt Metric) string {
	if mt.Name != "" {
		return mt.Name
	}
	if mt.Field == "" {
		return strings.ToLower(mt.Op)
	}
	return strings.ToLower(mt.Op) + "_" + mt.Field
}

// AggQuery aggregates live objects matching Where. RestrictPKs, when non-nil, limits the
// aggregation to those primary keys (used to aggregate only objects the caller may see).
type AggQuery struct {
	Where       *Where
	GroupBy     []string
	Metrics     []Metric
	Limit       int
	RestrictPKs []string
}

// AggRow is one aggregation group.
type AggRow struct {
	Group   map[string]any `json:"group,omitempty"`
	Metrics map[string]any `json:"metrics"`
}

func valueExpr(alias string, c column) string {
	switch {
	case c.prop.Type.Base == spec.TDecimal, c.prop.Type.Base == spec.TDate, isJSONType(c.prop.Type):
		return ident(alias, c.name) + "::text"
	}
	return ident(alias, c.name)
}

type metricCol struct {
	name string
	op   string
	prop *spec.Property
}

// Aggregate computes metrics per group; the bool reports that more groups than Limit exist.
func (s *Store) Aggregate(ctx context.Context, ont *spec.CompiledOntology, typ string, q AggQuery) ([]AggRow, bool, error) {
	m, err := meta(ont, typ)
	if err != nil {
		return nil, false, err
	}
	if len(q.Metrics) == 0 {
		q.Metrics = []Metric{{Op: "count"}}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultAggLimit
	}
	if limit > MaxAggLimit {
		limit = MaxAggLimit
	}
	b := &builder{ont: ont}
	where, err := b.cond(m, "t0", q.Where, 0)
	if err != nil {
		return nil, false, err
	}
	if q.RestrictPKs != nil {
		where = "(" + where + ") AND " + ident("t0", "_pk") + " = ANY(" + b.raw(q.RestrictPKs, "::text[]") + ")"
	}
	var sel []string
	groups := make([]column, 0, len(q.GroupBy))
	for _, g := range q.GroupBy {
		c, err := b.column(m, g)
		if err != nil {
			return nil, false, err
		}
		groups = append(groups, c)
		sel = append(sel, valueExpr("t0", c))
	}
	metrics := make([]metricCol, 0, len(q.Metrics))
	seen := map[string]bool{}
	for _, mt := range q.Metrics {
		name := MetricName(mt)
		if seen[name] {
			return nil, false, fmt.Errorf("%w: duplicate metric name %q", ErrInvalidQuery, name)
		}
		seen[name] = true
		op := strings.ToLower(mt.Op)
		mc := metricCol{name: name, op: op}
		var expr string
		switch op {
		case "count":
			expr = "count(*)"
			if mt.Field != "" {
				c, err := b.column(m, mt.Field)
				if err != nil {
					return nil, false, err
				}
				expr = "count(" + ident("t0", c.name) + ")"
			}
		case "sum", "avg", "min", "max":
			if mt.Field == "" {
				return nil, false, fmt.Errorf("%w: %s needs a field", ErrInvalidQuery, op)
			}
			c, err := b.column(m, mt.Field)
			if err != nil {
				return nil, false, err
			}
			mc.prop = c.prop
			base := c.prop.Type.Base
			if op == "sum" || op == "avg" {
				if !numeric(base) {
					return nil, false, fmt.Errorf("%w: %s needs a numeric field, %s is %s", ErrInvalidQuery, op, mt.Field, c.prop.Type)
				}
				cast := "::double precision"
				if base == spec.TDecimal {
					cast = "::text" // keep decimal precision
				}
				expr = op + "(" + ident("t0", c.name) + ")" + cast
			} else {
				if !orderable(base) {
					return nil, false, fmt.Errorf("%w: %s is not supported on %s (%s)", ErrInvalidQuery, op, mt.Field, c.prop.Type)
				}
				expr = op + "(" + ident("t0", c.name) + ")"
				if base == spec.TDecimal || base == spec.TDate {
					expr += "::text"
				}
			}
		default:
			return nil, false, fmt.Errorf("%w: unknown metric %q (count, sum, avg, min, max)", ErrInvalidQuery, mt.Op)
		}
		metrics = append(metrics, mc)
		sel = append(sel, expr)
	}
	clause := ""
	if len(groups) > 0 {
		pos := make([]string, len(groups))
		for i := range groups {
			pos[i] = fmt.Sprint(i + 1)
		}
		clause = " GROUP BY " + strings.Join(pos, ", ") + " ORDER BY " + strings.Join(pos, ", ")
	}
	sql := fmt.Sprintf(`SELECT %s FROM %s %s WHERE NOT %s AND (%s)%s LIMIT %d`,
		strings.Join(sel, ", "), m.ident, ident("t0"), ident("t0", "_deleted"), where, clause, limit+1)
	rows, err := s.db.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, false, fmt.Errorf("aggregate %s: %w", typ, err)
	}
	defer rows.Close()
	var out []AggRow
	for rows.Next() {
		vals := make([]any, len(sel))
		dest := make([]any, len(sel))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, false, err
		}
		row := AggRow{Metrics: make(map[string]any, len(metrics))}
		if len(groups) > 0 {
			row.Group = make(map[string]any, len(groups))
			for i, g := range groups {
				row.Group[g.prop.APIName] = decodeValue(g.prop, vals[i])
			}
		}
		for j, mc := range metrics {
			v := vals[len(groups)+j]
			if mc.prop != nil && (mc.op == "min" || mc.op == "max") {
				v = decodeValue(mc.prop, v)
			}
			row.Metrics[mc.name] = v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
