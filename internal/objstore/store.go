package objstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acme/ontology/internal/spec"
)

// Sentinel errors; match with errors.Is.
var (
	ErrNotFound      = errors.New("object not found")
	ErrStale         = errors.New("stale object")
	ErrAlreadyExists = errors.New("object already exists")
	ErrInvalidEdit   = errors.New("invalid edit")
	ErrUnknownType   = errors.New("unknown type")
)

// DB is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ObjectRef identifies an object by type apiName and primary key.
type ObjectRef struct{ Type, PK string }

func (r ObjectRef) String() string { return r.Type + "/" + r.PK }

// Object is a materialized object.
type Object struct {
	Ref         ObjectRef
	RID         string
	Version     int64
	Props       map[string]any // keyed by property apiName
	EditedProps []string
	Deleted     bool
}

// EditOp is the kind of an edit.
type EditOp string

const (
	OpCreate EditOp = "CREATE"
	OpModify EditOp = "MODIFY"
	OpDelete EditOp = "DELETE"
	OpLink   EditOp = "LINK"
	OpUnlink EditOp = "UNLINK"
)

// Edit is one change produced by an action.
type Edit struct {
	Op EditOp
	// Ref is the edited object (CREATE/MODIFY/DELETE) or the link's From-side object (LINK/UNLINK).
	Ref ObjectRef
	// Changes maps property apiName -> value (CREATE/MODIFY).
	Changes map[string]any
	// ExpectedVersion enables optimistic locking (MODIFY/DELETE, and the FK holder of LINK/UNLINK). 0 disables it.
	ExpectedVersion int64
	// LinkType and Target (the To-side object) are used by LINK/UNLINK.
	LinkType string
	Target   ObjectRef
}

// EditError reports which edit of a batch failed.
type EditError struct {
	Index int
	Edit  Edit
	Err   error
}

func (e *EditError) Error() string {
	return fmt.Sprintf("edit #%d %s %s: %v", e.Index, e.Edit.Op, e.Edit.Ref, e.Err)
}

func (e *EditError) Unwrap() error { return e.Err }

// Filter is the minimal predicate set supported in S1 (AND of all conditions).
type Filter struct {
	Eq map[string]any
	In map[string][]any
}

// Page is keyset pagination ordered by primary key.
type Page struct {
	Size  int
	Token string
}

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Store reads and writes object tables.
type Store struct{ db DB }

// New returns a Store using db for reads.
func New(db DB) *Store { return &Store{db: db} }

type column struct {
	prop *spec.Property
	name string
}

type tableMeta struct {
	ot     *spec.ObjectType
	ident  string
	cols   []column
	byProp map[string]column
}

func meta(ont *spec.CompiledOntology, typ string) (*tableMeta, error) {
	ot, ok := ont.ObjectTypes[typ]
	if !ok {
		return nil, fmt.Errorf("%w: object type %q", ErrUnknownType, typ)
	}
	cols, err := storedColumns(ot)
	if err != nil {
		return nil, err
	}
	m := &tableMeta{ot: ot, ident: tableIdent(ot.TableName), cols: cols, byProp: make(map[string]column, len(cols))}
	for _, c := range cols {
		m.byProp[c.prop.APIName] = c
	}
	return m, nil
}

func (m *tableMeta) lookup(name string) (column, error) {
	if c, ok := m.byProp[name]; ok {
		return c, nil
	}
	if _, ok := m.ot.Properties[name]; ok {
		return column{}, fmt.Errorf("%w: property %q is not stored in the object table", ErrInvalidEdit, name)
	}
	return column{}, fmt.Errorf("%w: unknown property %q on %s", ErrInvalidEdit, name, m.ot.APIName)
}

func (m *tableMeta) selectList() string {
	parts := []string{`"_pk"`, `"_rid"`, `"_version"`, `"_edited_props"`, `"_deleted"`}
	for _, c := range m.cols {
		parts = append(parts, selectExpr(c))
	}
	return strings.Join(parts, ", ")
}

func (m *tableMeta) scan(scan func(dest ...any) error) (*Object, error) {
	o := &Object{Props: make(map[string]any, len(m.cols))}
	vals := make([]any, len(m.cols))
	dest := []any{&o.Ref.PK, &o.RID, &o.Version, &o.EditedProps, &o.Deleted}
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	if err := scan(dest...); err != nil {
		return nil, err
	}
	o.Ref.Type = m.ot.APIName
	for i, c := range m.cols {
		o.Props[c.prop.APIName] = decodeValue(c.prop, vals[i])
	}
	return o, nil
}

// ---------- reads ----------

// Get returns a live (non-deleted) object or ErrNotFound.
func (s *Store) Get(ctx context.Context, ont *spec.CompiledOntology, ref ObjectRef) (*Object, error) {
	return getOne(ctx, s.db, ont, ref)
}

func getOne(ctx context.Context, db DB, ont *spec.CompiledOntology, ref ObjectRef) (*Object, error) {
	res, err := getMany(ctx, db, ont, []ObjectRef{ref})
	if err != nil {
		return nil, err
	}
	o, ok := res[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return o, nil
}

// GetMany returns the live objects found; missing refs are absent from the map.
func (s *Store) GetMany(ctx context.Context, ont *spec.CompiledOntology, refs []ObjectRef) (map[ObjectRef]*Object, error) {
	return getMany(ctx, s.db, ont, refs)
}

func getMany(ctx context.Context, db DB, ont *spec.CompiledOntology, refs []ObjectRef) (map[ObjectRef]*Object, error) {
	byType := map[string][]string{}
	for _, r := range refs {
		byType[r.Type] = append(byType[r.Type], r.PK)
	}
	out := make(map[ObjectRef]*Object, len(refs))
	for _, typ := range sortedKeys(byType) {
		m, err := meta(ont, typ)
		if err != nil {
			return nil, err
		}
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE "_pk" = ANY($1) AND NOT "_deleted"`, m.selectList(), m.ident)
		rows, err := db.Query(ctx, q, byType[typ])
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", typ, err)
		}
		for rows.Next() {
			o, err := m.scan(rows.Scan)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[o.Ref] = o
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Find returns live objects matching f, ordered by primary key, and the next page token ("" at the end).
func (s *Store) Find(ctx context.Context, ont *spec.CompiledOntology, typ string, f Filter, page Page) ([]*Object, string, error) {
	return find(ctx, s.db, ont, typ, f, page)
}

func find(ctx context.Context, db DB, ont *spec.CompiledOntology, typ string, f Filter, page Page) ([]*Object, string, error) {
	m, err := meta(ont, typ)
	if err != nil {
		return nil, "", err
	}
	size := page.Size
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	conds := []string{`NOT "_deleted"`}
	var args []any
	for _, name := range sortedKeys(f.Eq) {
		c, err := m.lookup(name)
		if err != nil {
			return nil, "", err
		}
		v, err := encodeValue(c.prop, f.Eq[name])
		if err != nil {
			return nil, "", err
		}
		if v == nil {
			conds = append(conds, ident(c.name)+" IS NULL")
			continue
		}
		args = append(args, v)
		conds = append(conds, ident(c.name)+" = "+placeholder(c.prop, len(args)))
	}
	for _, name := range sortedKeys(f.In) {
		c, err := m.lookup(name)
		if err != nil {
			return nil, "", err
		}
		if len(f.In[name]) == 0 {
			conds = append(conds, "false")
			continue
		}
		var phs []string
		for _, raw := range f.In[name] {
			v, err := encodeValue(c.prop, raw)
			if err != nil {
				return nil, "", err
			}
			args = append(args, v)
			phs = append(phs, placeholder(c.prop, len(args)))
		}
		conds = append(conds, ident(c.name)+" IN ("+strings.Join(phs, ", ")+")")
	}
	if page.Token != "" {
		args = append(args, page.Token)
		conds = append(conds, fmt.Sprintf(`"_pk" > $%d`, len(args)))
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY "_pk" LIMIT %d`,
		m.selectList(), m.ident, strings.Join(conds, " AND "), size+1)
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query %s: %w", typ, err)
	}
	defer rows.Close()
	var out []*Object
	for rows.Next() {
		o, err := m.scan(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > size {
		out = out[:size]
		next = out[size-1].Ref.PK
	}
	return out, next, nil
}

type resolvedLink struct {
	lt       *spec.LinkType
	fromSide bool // navigating from the link's From side
	target   string
}

func resolveLink(ont *spec.CompiledOntology, objType, nav string) (*resolvedLink, error) {
	for _, name := range sortedKeys(ont.LinkTypes) {
		lt := ont.LinkTypes[name]
		if lt.From.ObjectType == objType && lt.From.APIName == nav {
			return &resolvedLink{lt: lt, fromSide: true, target: lt.To.ObjectType}, nil
		}
		if lt.To.ObjectType == objType && lt.To.APIName == nav {
			return &resolvedLink{lt: lt, fromSide: false, target: lt.From.ObjectType}, nil
		}
	}
	return nil, fmt.Errorf("%w: link %q on %s", ErrUnknownType, nav, objType)
}

// fkHolderIsFrom reports whether the From-side object stores the foreign key.
func fkHolderIsFrom(lt *spec.LinkType) bool {
	if lt.From.ObjectType != lt.To.ObjectType {
		return lt.ForeignKey.On == lt.From.ObjectType
	}
	return lt.From.Cardinality == spec.CardOne
}

// Links traverses the navigation name nav from object from (e.g. Aircraft "workOrders").
func (s *Store) Links(ctx context.Context, ont *spec.CompiledOntology, from ObjectRef, nav string, limit int) ([]*Object, error) {
	rl, err := resolveLink(ont, from.Type, nav)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultPageSize
	}
	if isManyToMany(rl.lt) {
		self, other := "from_pk", "to_pk"
		if !rl.fromSide {
			self, other = other, self
		}
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1 AND NOT "_deleted" ORDER BY 1 LIMIT %d`,
			ident(other), tableIdent(LinkTableName(rl.lt)), ident(self), limit)
		rows, err := s.db.Query(ctx, q, from.PK)
		if err != nil {
			return nil, err
		}
		pks, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		return s.orderedGet(ctx, ont, rl.target, pks)
	}

	fk := rl.lt.ForeignKey
	srcIsHolder := rl.fromSide == fkHolderIsFrom(rl.lt)
	if srcIsHolder {
		src, err := s.Get(ctx, ont, from)
		if err != nil {
			return nil, err
		}
		v := src.Props[fk.Property]
		if v == nil {
			return nil, nil
		}
		return s.orderedGet(ctx, ont, rl.target, []string{pkString(v)})
	}
	holder, err := meta(ont, rl.target)
	if err != nil {
		return nil, err
	}
	pv, err := pkToValue(holder.ot.Properties[fk.Property], from.PK)
	if err != nil {
		return nil, err
	}
	objs, _, err := s.Find(ctx, ont, rl.target, Filter{Eq: map[string]any{fk.Property: pv}}, Page{Size: limit})
	return objs, err
}

func (s *Store) orderedGet(ctx context.Context, ont *spec.CompiledOntology, typ string, pks []string) ([]*Object, error) {
	refs := make([]ObjectRef, len(pks))
	for i, pk := range pks {
		refs[i] = ObjectRef{Type: typ, PK: pk}
	}
	found, err := s.GetMany(ctx, ont, refs)
	if err != nil {
		return nil, err
	}
	out := make([]*Object, 0, len(found))
	for _, r := range refs {
		if o, ok := found[r]; ok {
			out = append(out, o)
		}
	}
	return out, nil
}

// ---------- writes ----------

// ApplyEdits applies edits in order inside tx and returns the new version of every edited object.
// The caller owns the transaction: on error it MUST roll back (earlier edits may already be applied in tx).
// Multiple edits of the same object chain their versions automatically.
func (s *Store) ApplyEdits(ctx context.Context, tx DB, ont *spec.CompiledOntology, edits []Edit) (map[ObjectRef]int64, error) {
	out := map[ObjectRef]int64{}
	expected := func(ref ObjectRef, given int64) int64 {
		if given != 0 {
			if v, ok := out[ref]; ok {
				return v
			}
		}
		return given
	}
	for i, e := range edits {
		ref, ver, err := applyOne(ctx, tx, ont, e, expected)
		if err != nil {
			return nil, &EditError{Index: i, Edit: e, Err: err}
		}
		if ver != 0 {
			out[ref] = ver
		}
	}
	return out, nil
}

func applyOne(ctx context.Context, tx DB, ont *spec.CompiledOntology, e Edit, expected func(ObjectRef, int64) int64) (ObjectRef, int64, error) {
	switch e.Op {
	case OpCreate, OpModify, OpDelete:
		if e.Ref.PK == "" {
			return e.Ref, 0, fmt.Errorf("%w: primary key is required", ErrInvalidEdit)
		}
		m, err := meta(ont, e.Ref.Type)
		if err != nil {
			return e.Ref, 0, err
		}
		var ver int64
		switch e.Op {
		case OpCreate:
			ver, err = applyCreate(ctx, tx, m, e.Ref, e.Changes)
		case OpModify:
			ver, err = applyModify(ctx, tx, m, e.Ref, e.Changes, expected(e.Ref, e.ExpectedVersion))
		default:
			ver, err = applyDelete(ctx, tx, m, e.Ref, expected(e.Ref, e.ExpectedVersion))
		}
		return e.Ref, ver, err
	case OpLink, OpUnlink:
		return applyLink(ctx, tx, ont, e, expected)
	}
	return e.Ref, 0, fmt.Errorf("%w: unknown op %q", ErrInvalidEdit, e.Op)
}

func newRID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("ri.object.%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func propNames(changes map[string]any) []string {
	out := sortedKeys(changes)
	sort.Strings(out)
	return out
}

// checkPK verifies that a primary key value in changes (if any) equals ref.PK.
func checkPK(m *tableMeta, ref ObjectRef, changes map[string]any) error {
	v, ok := changes[m.ot.PrimaryKey]
	if !ok {
		return nil
	}
	enc, err := encodeValue(m.ot.Properties[m.ot.PrimaryKey], v)
	if err != nil {
		return err
	}
	if enc == nil || pkString(enc) != ref.PK {
		return fmt.Errorf("%w: primary key %q cannot be changed or differ from the object reference", ErrInvalidEdit, m.ot.PrimaryKey)
	}
	return nil
}

func applyCreate(ctx context.Context, tx DB, m *tableMeta, ref ObjectRef, changes map[string]any) (int64, error) {
	if err := checkPK(m, ref, changes); err != nil {
		return 0, err
	}
	pkProp := m.ot.Properties[m.ot.PrimaryKey]
	pkVal, err := pkToValue(pkProp, ref.PK)
	if err != nil {
		return 0, err
	}
	all := make(map[string]any, len(changes)+1)
	for k, v := range changes {
		all[k] = v
	}
	all[m.ot.PrimaryKey] = pkVal

	cols := []string{`"_pk"`, `"_rid"`, `"_edit_ts"`, `"_edited_props"`}
	vals := []string{"$1", "$2", "now()", "$3::text[]"}
	args := []any{ref.PK, newRID(), propNames(changes)}
	for _, name := range propNames(all) {
		c, err := m.lookup(name)
		if err != nil {
			return 0, err
		}
		v, err := encodeValue(c.prop, all[name])
		if err != nil {
			return 0, err
		}
		args = append(args, v)
		cols = append(cols, ident(c.name))
		vals = append(vals, placeholder(c.prop, len(args)))
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT ("_pk") DO NOTHING RETURNING "_version"`,
		m.ident, strings.Join(cols, ", "), strings.Join(vals, ", "))
	var ver int64
	err = tx.QueryRow(ctx, q, args...).Scan(&ver)
	if errors.Is(err, pgx.ErrNoRows) {
		// Also covers soft-deleted rows: re-creating a deleted primary key is not allowed in S1.
		return 0, fmt.Errorf("%w: %s", ErrAlreadyExists, ref)
	}
	return ver, err
}

const lockCond = `"_pk" = $1 AND NOT "_deleted" AND ($2::bigint = 0 OR "_version" = $2::bigint)`

func applyModify(ctx context.Context, tx DB, m *tableMeta, ref ObjectRef, changes map[string]any, expected int64) (int64, error) {
	if len(changes) == 0 {
		return 0, fmt.Errorf("%w: modify requires at least one change", ErrInvalidEdit)
	}
	if err := checkPK(m, ref, changes); err != nil {
		return 0, err
	}
	var names []string
	for _, n := range propNames(changes) {
		if n != m.ot.PrimaryKey {
			names = append(names, n)
		}
	}
	args := []any{ref.PK, expected, names}
	var sets []string
	for _, name := range names {
		c, err := m.lookup(name)
		if err != nil {
			return 0, err
		}
		v, err := encodeValue(c.prop, changes[name])
		if err != nil {
			return 0, err
		}
		args = append(args, v)
		sets = append(sets, ident(c.name)+" = "+placeholder(c.prop, len(args)))
	}
	sets = append(sets,
		`"_version" = "_version" + 1`,
		`"_edit_ts" = now()`,
		`"_updated_at" = now()`,
		`"_edited_props" = ARRAY(SELECT DISTINCT p FROM unnest("_edited_props" || $3::text[]) AS p ORDER BY p)`,
	)
	q := fmt.Sprintf(`UPDATE %s SET %s WHERE %s RETURNING "_version"`, m.ident, strings.Join(sets, ", "), lockCond)
	var ver int64
	err := tx.QueryRow(ctx, q, args...).Scan(&ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, classify(ctx, tx, m, ref)
	}
	return ver, err
}

func applyDelete(ctx context.Context, tx DB, m *tableMeta, ref ObjectRef, expected int64) (int64, error) {
	q := fmt.Sprintf(`UPDATE %s SET "_deleted" = true, "_version" = "_version" + 1, "_edit_ts" = now(), "_updated_at" = now() WHERE %s RETURNING "_version"`,
		m.ident, lockCond)
	var ver int64
	err := tx.QueryRow(ctx, q, ref.PK, expected).Scan(&ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, classify(ctx, tx, m, ref)
	}
	return ver, err
}

// classify explains why a guarded UPDATE matched no row.
func classify(ctx context.Context, tx DB, m *tableMeta, ref ObjectRef) error {
	var deleted bool
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT "_deleted" FROM %s WHERE "_pk" = $1`, m.ident), ref.PK).Scan(&deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && deleted:
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	case err != nil:
		return err
	}
	return fmt.Errorf("%w: %s", ErrStale, ref)
}

func applyLink(ctx context.Context, tx DB, ont *spec.CompiledOntology, e Edit, expected func(ObjectRef, int64) int64) (ObjectRef, int64, error) {
	lt, ok := ont.LinkTypes[e.LinkType]
	if !ok {
		return e.Ref, 0, fmt.Errorf("%w: link type %q", ErrUnknownType, e.LinkType)
	}
	if e.Ref.Type != lt.From.ObjectType || e.Target.Type != lt.To.ObjectType {
		return e.Ref, 0, fmt.Errorf("%w: link %s connects %s -> %s, got %s -> %s",
			ErrInvalidEdit, lt.APIName, lt.From.ObjectType, lt.To.ObjectType, e.Ref.Type, e.Target.Type)
	}
	if e.Ref.PK == "" || e.Target.PK == "" {
		return e.Ref, 0, fmt.Errorf("%w: both link ends need a primary key", ErrInvalidEdit)
	}

	if isManyToMany(lt) {
		t := tableIdent(LinkTableName(lt))
		if e.Op == OpLink {
			found, err := getMany(ctx, tx, ont, []ObjectRef{e.Ref, e.Target})
			if err != nil {
				return e.Ref, 0, err
			}
			for _, r := range []ObjectRef{e.Ref, e.Target} {
				if _, ok := found[r]; !ok {
					return e.Ref, 0, fmt.Errorf("%w: %s", ErrNotFound, r)
				}
			}
			_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s ("from_pk", "to_pk", "_source", "_deleted") VALUES ($1, $2, 'EDIT', false)
ON CONFLICT ("from_pk", "to_pk") DO UPDATE SET "_deleted" = false, "_source" = 'EDIT', "_updated_at" = now()`, t), e.Ref.PK, e.Target.PK)
			return e.Ref, 0, err
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET "_deleted" = true, "_source" = 'EDIT', "_updated_at" = now()
WHERE "from_pk" = $1 AND "to_pk" = $2 AND NOT "_deleted"`, t), e.Ref.PK, e.Target.PK)
		if err == nil && tag.RowsAffected() == 0 {
			err = fmt.Errorf("%w: link %s %s -> %s", ErrNotFound, lt.APIName, e.Ref, e.Target)
		}
		return e.Ref, 0, err
	}

	holder, target := e.Ref, e.Target
	if !fkHolderIsFrom(lt) {
		holder, target = target, holder
	}
	hm, err := meta(ont, holder.Type)
	if err != nil {
		return holder, 0, err
	}
	fkProp := lt.ForeignKey.Property
	if e.Op == OpLink {
		if _, err := getOne(ctx, tx, ont, target); err != nil {
			return holder, 0, err
		}
		v, err := pkToValue(hm.ot.Properties[fkProp], target.PK)
		if err != nil {
			return holder, 0, err
		}
		ver, err := applyModify(ctx, tx, hm, holder, map[string]any{fkProp: v}, expected(holder, e.ExpectedVersion))
		return holder, ver, err
	}
	cur, err := getOne(ctx, tx, ont, holder)
	if err != nil {
		return holder, 0, err
	}
	if v := cur.Props[fkProp]; v == nil || pkString(v) != target.PK {
		return holder, 0, fmt.Errorf("%w: link %s %s -> %s", ErrNotFound, lt.APIName, e.Ref, e.Target)
	}
	ver, err := applyModify(ctx, tx, hm, holder, map[string]any{fkProp: nil}, expected(holder, e.ExpectedVersion))
	return holder, ver, err
}
