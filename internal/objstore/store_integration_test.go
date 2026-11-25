//go:build integration

package objstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/spec"
)

// Requires ONTO_TEST_DSN pointing at a disposable database: the test drops and recreates schema "obj".
func setup(t *testing.T) (context.Context, *pgxpool.Pool, *spec.CompiledOntology, *Store) {
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
	ont := loadExample(t)
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS "obj" CASCADE`); err != nil {
		t.Fatal(err)
	}
	stmts, err := GenerateDDL(ont)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	return ctx, pool, ont, New(pool)
}

func apply(ctx context.Context, pool *pgxpool.Pool, s *Store, ont *spec.CompiledOntology, edits ...Edit) (map[ObjectRef]int64, error) {
	var out map[ObjectRef]int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.ApplyEdits(ctx, tx, ont, edits)
		return err
	})
	return out, err
}

func wo(pk string) ObjectRef { return ObjectRef{Type: "WorkOrder", PK: pk} }

func newWO(pk string) Edit {
	return Edit{Op: OpCreate, Ref: wo(pk), Changes: map[string]any{
		"summary": "check", "status": "OPEN", "priority": 2, "tailNumber": "B-1234",
		"estimatedCost": "1200.50", "dueAt": time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	}}
}

func TestCreateModifyDelete(t *testing.T) {
	ctx, pool, ont, s := setup(t)
	vers, err := apply(ctx, pool, s, ont, newWO("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	if vers[wo("WO-1")] != 1 {
		t.Fatalf("version = %d", vers[wo("WO-1")])
	}
	o, err := s.Get(ctx, ont, wo("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Props["priority"] != int64(2) || o.Props["estimatedCost"] != "1200.50" || o.Props["workOrderId"] != "WO-1" {
		t.Errorf("props = %#v", o.Props)
	}
	if due, _ := o.Props["dueAt"].(time.Time); !due.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dueAt = %v", o.Props["dueAt"])
	}

	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpModify, Ref: wo("WO-1"), ExpectedVersion: 1, Changes: map[string]any{"priority": 1}}); err != nil {
		t.Fatal(err)
	}
	o, _ = s.Get(ctx, ont, wo("WO-1"))
	if o.Version != 2 || o.Props["priority"] != int64(1) {
		t.Errorf("after modify: v=%d props=%v", o.Version, o.Props)
	}
	if len(o.EditedProps) == 0 {
		t.Error("edited props not tracked")
	}

	_, err = apply(ctx, pool, s, ont, Edit{Op: OpModify, Ref: wo("WO-1"), ExpectedVersion: 1, Changes: map[string]any{"priority": 3}})
	if !errors.Is(err, ErrStale) {
		t.Errorf("expected ErrStale, got %v", err)
	}
	if _, err := apply(ctx, pool, s, ont, newWO("WO-1")); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists, got %v", err)
	}
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpDelete, Ref: wo("WO-1"), ExpectedVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, ont, wo("WO-1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpModify, Ref: wo("WO-1"), Changes: map[string]any{"priority": 1}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestBatchIsAtomic(t *testing.T) {
	ctx, pool, ont, s := setup(t)
	if _, err := apply(ctx, pool, s, ont, newWO("WO-A")); err != nil {
		t.Fatal(err)
	}
	_, err := apply(ctx, pool, s, ont,
		Edit{Op: OpModify, Ref: wo("WO-A"), ExpectedVersion: 1, Changes: map[string]any{"priority": 5}},
		Edit{Op: OpModify, Ref: wo("WO-A"), ExpectedVersion: 1, Changes: map[string]any{"noSuchProp": 1}},
	)
	var ee *EditError
	if !errors.As(err, &ee) || ee.Index != 1 || !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("expected EditError #1 ErrInvalidEdit, got %v", err)
	}
	o, _ := s.Get(ctx, ont, wo("WO-A"))
	if o.Props["priority"] != int64(2) || o.Version != 1 {
		t.Errorf("batch not rolled back: v=%d priority=%v", o.Version, o.Props["priority"])
	}
	// Chained versions: two modifies of the same object in one batch both expecting v1.
	if _, err := apply(ctx, pool, s, ont,
		Edit{Op: OpModify, Ref: wo("WO-A"), ExpectedVersion: 1, Changes: map[string]any{"priority": 4}},
		Edit{Op: OpModify, Ref: wo("WO-A"), ExpectedVersion: 1, Changes: map[string]any{"status": "IN_PROGRESS"}},
	); err != nil {
		t.Fatalf("chained modifies failed: %v", err)
	}
}

func TestConcurrentModifyExactlyOneWins(t *testing.T) {
	ctx, pool, ont, s := setup(t)
	if _, err := apply(ctx, pool, s, ont, newWO("WO-C")); err != nil {
		t.Fatal(err)
	}
	const n = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, stale := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := apply(ctx, pool, s, ont, Edit{Op: OpModify, Ref: wo("WO-C"), ExpectedVersion: 1, Changes: map[string]any{"priority": i%5 + 1}})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrStale):
				stale++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || stale != n-1 {
		t.Fatalf("wins=%d stale=%d", wins, stale)
	}
}

func TestLinks(t *testing.T) {
	ctx, pool, ont, s := setup(t)
	ac := ObjectRef{Type: "Aircraft", PK: "B-1234"}
	tech := ObjectRef{Type: "Technician", PK: "T-1"}
	part := ObjectRef{Type: "Part", PK: "P-1"}
	_, err := apply(ctx, pool, s, ont,
		Edit{Op: OpCreate, Ref: ac, Changes: map[string]any{"baseAirport": "SIN", "status": "ACTIVE"}},
		Edit{Op: OpCreate, Ref: tech, Changes: map[string]any{"name": "Lee", "certifications": []string{"A320"}}},
		Edit{Op: OpCreate, Ref: part, Changes: map[string]any{"partNumber": "PN-9"}},
		newWO("WO-2"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// FK link: WorkOrder (From) -> Technician (To); WorkOrder holds technicianId.
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpLink, LinkType: "workOrderTechnician", Ref: wo("WO-2"), Target: tech}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Links(ctx, ont, tech, "workOrders", 10)
	if err != nil || len(got) != 1 || got[0].Ref != wo("WO-2") {
		t.Fatalf("Technician.workOrders = %v, %v", got, err)
	}
	got, err = s.Links(ctx, ont, wo("WO-2"), "technician", 10)
	if err != nil || len(got) != 1 || got[0].Ref != tech {
		t.Fatalf("WorkOrder.technician = %v, %v", got, err)
	}
	got, _ = s.Links(ctx, ont, ac, "workOrders", 10)
	if len(got) != 1 {
		t.Errorf("Aircraft.workOrders = %d objects", len(got))
	}
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpUnlink, LinkType: "workOrderTechnician", Ref: wo("WO-2"), Target: tech}); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Get(ctx, ont, wo("WO-2")); o.Props["technicianId"] != nil {
		t.Errorf("technicianId after unlink = %v", o.Props["technicianId"])
	}

	// M:N link: Aircraft (From) <-> Part (To).
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpLink, LinkType: "aircraftParts", Ref: ac, Target: part}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Links(ctx, ont, part, "installedOn", 10)
	if len(got) != 1 || got[0].Ref != ac {
		t.Fatalf("Part.installedOn = %v", got)
	}
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpUnlink, LinkType: "aircraftParts", Ref: ac, Target: part}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Links(ctx, ont, ac, "parts", 10)
	if len(got) != 0 {
		t.Errorf("Aircraft.parts after unlink = %v", got)
	}
	if _, err := apply(ctx, pool, s, ont, Edit{Op: OpLink, LinkType: "aircraftParts", Ref: ac, Target: ObjectRef{Type: "Part", PK: "nope"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("link to missing part: expected ErrNotFound, got %v", err)
	}
}

func TestFindPagination(t *testing.T) {
	ctx, pool, ont, s := setup(t)
	var edits []Edit
	for _, pk := range []string{"WO-a", "WO-b", "WO-c"} {
		edits = append(edits, newWO(pk))
	}
	if _, err := apply(ctx, pool, s, ont, edits...); err != nil {
		t.Fatal(err)
	}
	page1, next, err := s.Find(ctx, ont, "WorkOrder", Filter{Eq: map[string]any{"status": "OPEN"}}, Page{Size: 2})
	if err != nil || len(page1) != 2 || next != "WO-b" {
		t.Fatalf("page1 = %d next=%q err=%v", len(page1), next, err)
	}
	page2, next, _ := s.Find(ctx, ont, "WorkOrder", Filter{Eq: map[string]any{"status": "OPEN"}}, Page{Size: 2, Token: next})
	if len(page2) != 1 || next != "" {
		t.Fatalf("page2 = %d next=%q", len(page2), next)
	}
	none, _, _ := s.Find(ctx, ont, "WorkOrder", Filter{In: map[string][]any{"priority": {5}}}, Page{})
	if len(none) != 0 {
		t.Errorf("expected no results, got %d", len(none))
	}
}
