//go:build integration

package indexer

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// The source system is simulated by schema "public" tables in the same database; the example
// ontology maps them as public.aircraft, public.work_orders, public.aircraft_parts, ...
func setup(t *testing.T) (context.Context, *pgxpool.Pool, *spec.CompiledOntology, *Indexer) {
	t.Helper()
	dsn := os.Getenv("ONTO_TEST_DSN")
	if dsn == "" {
		t.Skip("ONTO_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ont := loadOntology(t)
	ddl, err := objstore.GenerateDDL(ont)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{`DROP SCHEMA IF EXISTS obj CASCADE`}, ddl...)
	stmts = append(stmts,
		`DELETE FROM indexer.sync_state`,
		`DROP TABLE IF EXISTS public.aircraft, public.work_orders, public.technicians, public.parts, public.aircraft_parts`,
		`CREATE TABLE public.aircraft (tail_no text PRIMARY KEY, model_code text, status text, total_fh numeric, base_iata text)`,
		`CREATE TABLE public.work_orders (work_order_id text PRIMARY KEY, summary text, description text, status text,
		   priority int, tail_number text, technician_id text, due_at timestamptz, estimated_cost numeric(14,2),
		   updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE public.technicians (technician_id text PRIMARY KEY, name text, email text, certifications jsonb, base_airport text)`,
		`CREATE TABLE public.parts (part_sn text PRIMARY KEY, part_number text, description text, remaining_life_hours double precision)`,
		`CREATE TABLE public.aircraft_parts (tail_no text, part_sn text)`,
	)
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	ix := &Indexer{DB: pool, Store: objstore.New(pool), Sources: map[string]Querier{"mro_db": pool}, BatchSize: 2}
	return ctx, pool, ont, ix
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func get(t *testing.T, ix *Indexer, ont *spec.CompiledOntology, typ, pk string) *objstore.Object {
	t.Helper()
	o, err := ix.Store.Get(context.Background(), ont, objstore.ObjectRef{Type: typ, PK: pk})
	if err != nil {
		return nil
	}
	return o
}

func userEdit(t *testing.T, pool *pgxpool.Pool, ix *Indexer, ont *spec.CompiledOntology, e objstore.Edit) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := ix.Store.ApplyEdits(context.Background(), tx, ont, []objstore.Edit{e})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFullSyncApplyUserEdits(t *testing.T) { // Aircraft: APPLY_USER_EDITS, no timestamp column
	ctx, pool, ont, ix := setup(t)
	exec(t, pool, `INSERT INTO public.aircraft VALUES ('B-1234','A320','ACTIVE',1000.5,'SIN'),('B-5678','B737','ACTIVE',20,'SIN'),('B-9999','A350','ACTIVE',5,'PEK')`)
	res, err := ix.SyncObjectType(ctx, ont, "Aircraft", false)
	if err != nil || res.Mode != "full" || res.Stats.Inserted != 3 {
		t.Fatalf("first sync = %+v, %v", res, err)
	}
	o := get(t, ix, ont, "Aircraft", "B-1234")
	if o == nil || o.Props["flightHours"] != 1000.5 || o.Props["model"] != "A320" {
		t.Fatalf("object = %+v", o)
	}

	// Unchanged source: nothing is written, versions stay put.
	res, _ = ix.SyncObjectType(ctx, ont, "Aircraft", false)
	if res.Stats.Unchanged != 3 || res.Stats.Updated != 0 || get(t, ix, ont, "Aircraft", "B-1234").Version != 1 {
		t.Fatalf("no-op sync = %+v", res.Stats)
	}

	// A user grounds B-1234; the source still says ACTIVE and changes the base.
	userEdit(t, pool, ix, ont, objstore.Edit{Op: objstore.OpModify, Ref: objstore.ObjectRef{Type: "Aircraft", PK: "B-1234"},
		Changes: map[string]any{"status": "GROUNDED"}})
	exec(t, pool, `UPDATE public.aircraft SET base_iata = 'KUL' WHERE tail_no = 'B-1234'`)
	ix.SyncObjectType(ctx, ont, "Aircraft", false) //nolint:errcheck
	o = get(t, ix, ont, "Aircraft", "B-1234")
	if o.Props["status"] != "GROUNDED" || o.Props["baseAirport"] != "KUL" {
		t.Errorf("user edit must win, source must update the rest: %v", o.Props)
	}

	// Source deletes B-5678 (soft delete); a user deletes B-9999 (must stay deleted).
	userEdit(t, pool, ix, ont, objstore.Edit{Op: objstore.OpDelete, Ref: objstore.ObjectRef{Type: "Aircraft", PK: "B-9999"}})
	exec(t, pool, `DELETE FROM public.aircraft WHERE tail_no = 'B-5678'`)
	res, _ = ix.SyncObjectType(ctx, ont, "Aircraft", false)
	if res.Stats.Deleted != 1 || get(t, ix, ont, "Aircraft", "B-5678") != nil || get(t, ix, ont, "Aircraft", "B-9999") != nil {
		t.Fatalf("delete handling: %+v", res.Stats)
	}

	// Source row returns: the source-deleted object comes back.
	exec(t, pool, `INSERT INTO public.aircraft VALUES ('B-5678','B737','ACTIVE',21,'SIN')`)
	ix.SyncObjectType(ctx, ont, "Aircraft", false) //nolint:errcheck
	if get(t, ix, ont, "Aircraft", "B-5678") == nil {
		t.Error("a source-deleted object must reappear when its row returns")
	}
}

func TestIncrementalMostRecentValue(t *testing.T) { // WorkOrder: MOST_RECENT_VALUE + updated_at
	ctx, pool, ont, ix := setup(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	for i, id := range []string{"WO-1", "WO-2", "WO-3"} {
		exec(t, pool, `INSERT INTO public.work_orders (work_order_id, summary, status, priority, tail_number, estimated_cost, updated_at)
VALUES ($1, 'check', 'OPEN', 3, 'B-1234', 100.50, $2)`, id, base.Add(time.Duration(i)*time.Minute))
	}
	if res, err := ix.SyncObjectType(ctx, ont, "WorkOrder", false); err != nil || res.Mode != "full" || res.Stats.Inserted != 3 {
		t.Fatalf("initial full sync = %+v, %v", res, err)
	}

	// A user sets priority 1 now; then the source changes the same work order.
	userEdit(t, pool, ix, ont, objstore.Edit{Op: objstore.OpModify, Ref: objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"},
		Changes: map[string]any{"priority": int64(1)}})
	exec(t, pool, `UPDATE public.work_orders SET status = 'IN_PROGRESS', updated_at = $1 WHERE work_order_id = 'WO-2'`, base.Add(-time.Hour)) // older than cursor: not picked up incrementally
	exec(t, pool, `UPDATE public.work_orders SET summary = 'recheck', updated_at = now() + interval '1 minute' WHERE work_order_id = 'WO-1'`)
	exec(t, pool, `INSERT INTO public.work_orders (work_order_id, summary, status, tail_number, updated_at) VALUES ('WO-4','new','OPEN','B-1234', now() + interval '2 minutes')`)

	res, err := ix.SyncObjectType(ctx, ont, "WorkOrder", false)
	if err != nil || res.Mode != "incremental" {
		t.Fatalf("incremental = %+v, %v", res, err)
	}
	if res.Stats.Inserted != 1 || res.Stats.Updated != 1 {
		t.Errorf("incremental stats = %+v (want 1 inserted, 1 updated)", res.Stats)
	}
	o := get(t, ix, ont, "WorkOrder", "WO-1")
	// The source row is newer than the edit, so MOST_RECENT_VALUE lets it win everywhere.
	if o.Props["summary"] != "recheck" || o.Props["priority"] != int64(3) {
		t.Errorf("WO-1 = %v", o.Props)
	}
	if o.Props["estimatedCost"] != "100.50" {
		t.Errorf("decimal round trip: %v", o.Props["estimatedCost"])
	}

	// A user edit newer than the source row keeps its value.
	userEdit(t, pool, ix, ont, objstore.Edit{Op: objstore.OpModify, Ref: objstore.ObjectRef{Type: "WorkOrder", PK: "WO-4"},
		Changes: map[string]any{"priority": int64(1)}})
	exec(t, pool, `UPDATE public.work_orders SET summary = 'stale', priority = 5, updated_at = now() - interval '1 hour' WHERE work_order_id = 'WO-4'`)
	ix.SyncObjectType(ctx, ont, "WorkOrder", true) //nolint:errcheck // full snapshot reads every row
	if o := get(t, ix, ont, "WorkOrder", "WO-4"); o.Props["priority"] != int64(1) || o.Props["summary"] != "stale" {
		t.Errorf("WO-4 = %v (edit newer than source must win; unedited summary follows the source)", o.Props)
	}
}

func TestDeleteGuardAndLinks(t *testing.T) {
	ctx, pool, ont, ix := setup(t)
	for i := 0; i < 12; i++ {
		exec(t, pool, `INSERT INTO public.parts VALUES ($1, 'PN', 'd', 100)`, "P-"+string(rune('a'+i)))
	}
	if _, err := ix.SyncObjectType(ctx, ont, "Part", false); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `DELETE FROM public.parts WHERE part_sn <> 'P-a'`)
	if _, err := ix.SyncObjectType(ctx, ont, "Part", false); !errors.Is(err, ErrDeleteGuard) {
		t.Fatalf("expected the delete guard, got %v", err)
	}
	if get(t, ix, ont, "Part", "P-b") == nil {
		t.Fatal("a refused full sync must not delete anything")
	}
	ix.Force = true
	if res, err := ix.SyncObjectType(ctx, ont, "Part", false); err != nil || res.Stats.Deleted != 11 {
		t.Fatalf("forced sync = %+v, %v", res, err)
	}

	exec(t, pool, `INSERT INTO public.aircraft_parts VALUES ('B-1','P-a'),('B-1','P-b'),('B-2','P-a')`)
	res, err := ix.SyncLinkType(ctx, ont, "aircraftParts")
	if err != nil || res.Stats.Inserted != 3 {
		t.Fatalf("link sync = %+v, %v", res, err)
	}
	exec(t, pool, `DELETE FROM public.aircraft_parts WHERE tail_no = 'B-2'`)
	res, _ = ix.SyncLinkType(ctx, ont, "aircraftParts")
	var live int
	pool.QueryRow(ctx, `SELECT count(*) FROM obj.link_aircraft_parts WHERE NOT _deleted`).Scan(&live) //nolint:errcheck
	if res.Stats.Deleted != 1 || live != 2 {
		t.Errorf("after source delete: stats=%+v live=%d", res.Stats, live)
	}
}

func TestApplyDebeziumEvent(t *testing.T) {
	ctx, pool, ont, ix := setup(t)
	_ = pool
	ev, err := ParseDebezium([]byte(`{"op":"c","after":{"tail_no":"B-7777","model_code":"A321","status":"ACTIVE","total_fh":12,"base_iata":"SIN"},
"source":{"schema":"public","table":"aircraft","ts_ms":1764039600000}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ix.ApplyEvent(ctx, ont, "mro_db", ev); !ok || err != nil {
		t.Fatalf("apply create = %v, %v", ok, err)
	}
	if o := get(t, ix, ont, "Aircraft", "B-7777"); o == nil || o.Props["model"] != "A321" {
		t.Fatalf("object = %+v", o)
	}
	del, _ := ParseDebezium([]byte(`{"op":"d","before":{"tail_no":"B-7777"},"source":{"table":"aircraft"}}`))
	if ok, err := ix.ApplyEvent(ctx, ont, "mro_db", del); !ok || err != nil {
		t.Fatalf("apply delete = %v, %v", ok, err)
	}
	if get(t, ix, ont, "Aircraft", "B-7777") != nil {
		t.Error("object must be soft-deleted")
	}
	other, _ := ParseDebezium([]byte(`{"op":"c","after":{"id":1},"source":{"table":"unmapped"}}`))
	if ok, _ := ix.ApplyEvent(ctx, ont, "mro_db", other); ok {
		t.Error("an unmapped table must be ignored")
	}
}
