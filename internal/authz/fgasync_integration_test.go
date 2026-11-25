//go:build integration

package authz

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

type memFGA struct {
	mu     sync.Mutex
	tuples map[Tuple]bool
}

func (m *memFGA) WriteTuples(_ context.Context, w, d []Tuple) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range d {
		delete(m.tuples, t)
	}
	for _, t := range w {
		m.tuples[t] = true
	}
	return nil
}

func (m *memFGA) list() []Tuple {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Tuple
	for t := range m.tuples {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Object+out[i].User < out[j].Object+out[j].User })
	return out
}

func TestFGASyncIntegration(t *testing.T) {
	dsn := os.Getenv("ONTO_TEST_DSN")
	if dsn == "" {
		t.Skip("ONTO_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatal(d.Codes())
	}
	ddl, _ := objstore.GenerateDDL(ont)
	for _, s := range append([]string{`DROP SCHEMA IF EXISTS obj CASCADE`, `DELETE FROM authz.derived_tuple`, `DELETE FROM authz.sync_cursor`,
		`TRUNCATE edits.side_effect_run, edits.outbox, edits.object_edit, edits.action_run`}, ddl...) {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	store := objstore.New(pool)
	fga := &memFGA{tuples: map[Tuple]bool{{"user:admin", "manager", "base:SIN"}: true}} // managed elsewhere
	s := &FGASync{DB: pool, Store: store, Writer: fga}
	apply := func(edits ...objstore.Edit) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { _, err := store.ApplyEdits(ctx, tx, ont, edits); return err }); err != nil {
			t.Fatal(err)
		}
	}
	wo := objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"}
	apply(objstore.Edit{Op: objstore.OpCreate, Ref: wo, Changes: map[string]any{"summary": "s", "status": "OPEN", "tailNumber": "B-1", "technicianId": "T-1"}})
	if w, d, err := s.FullSync(ctx, ont, "WorkOrder", 10); err != nil || w != 2 || d != 0 {
		t.Fatalf("full sync = %d/%d %v", w, d, err)
	}

	// Reassign through the outbox path.
	apply(objstore.Edit{Op: objstore.OpModify, Ref: wo, Changes: map[string]any{"technicianId": "T-2"}})
	if _, err := pool.Exec(ctx, `INSERT INTO edits.action_run (action_rid, ontology, ontology_ver, action_type, actor_id, actor_kind, request_hash, parameters, status)
VALUES ('r1','mro',1,'assign','alice','USER','h','{}','COMMITTED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO edits.outbox (aggregate, aggregate_id, event_type, payload) VALUES ('WorkOrder','WO-1','ObjectEdited','{}')`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ProcessOutbox(ctx, ont, "fga", 100); err != nil || n != 1 {
		t.Fatalf("outbox = %d %v", n, err)
	}
	want := []Tuple{{"user:admin", "manager", "base:SIN"}, {"aircraft:B-1", "aircraft", "work_order:WO-1"}, {"user:T-2", "assignee", "work_order:WO-1"}}
	sort.Slice(want, func(i, j int) bool { return want[i].Object+want[i].User < want[j].Object+want[j].User })
	if got := fga.list(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("tuples after reassign = %v", got)
	}
	if n, _ := s.ProcessOutbox(ctx, ont, "fga", 100); n != 0 {
		t.Errorf("cursor not advanced: %d", n)
	}

	// Deleting the object removes its derived tuples but keeps unrelated ones.
	apply(objstore.Edit{Op: objstore.OpDelete, Ref: wo})
	if _, d, err := s.FullSync(ctx, ont, "WorkOrder", 10); err != nil || d != 2 {
		t.Fatalf("after delete: deletes=%d err=%v", d, err)
	}
	if got := fga.list(); len(got) != 1 || got[0].Object != "base:SIN" {
		t.Errorf("remaining tuples = %v", got)
	}
}
