//go:build integration

package oms

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Requires ONTO_TEST_DSN on a disposable database with migrations applied (make dev-up).
// The test resets the oms/obj schemas between runs.
func setup(t *testing.T) (context.Context, *pgxpool.Pool, *Service, *Store) {
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
	for _, stmt := range []string{
		`TRUNCATE oms.ontology_version, oms.type_registry CASCADE`,
		`DELETE FROM oms.ontology`,
		`DROP SCHEMA IF EXISTS obj CASCADE`,
		`CREATE SCHEMA obj`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	store := NewStore(pool)
	return ctx, pool, NewService(store), store
}

const ontologyYAML = `ontology:
  apiName: mro
  displayName: MRO
  connectors: { mro_db: { kind: postgres, dsnRef: "vault://x" } }
`

func woYAML(extraProp string) string {
	return `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
    status: { type: string }
` + extraProp + `  datasource: { connector: mro_db, table: public.wo }
`
}

func files(wo string, extra ...SourceFile) []SourceFile {
	out := []SourceFile{{Path: "ontology.yaml", Content: ontologyYAML}, {Path: "wo.yaml", Content: wo}}
	return append(out, extra...)
}

func TestPublishLifecycle(t *testing.T) {
	ctx, pool, svc, store := setup(t)

	// First publish -> version 1, ACTIVE, obj.work_order created.
	res, err := svc.Publish(ctx, PublishInput{Ontology: "mro", GitCommit: "c1", Actor: "u", Files: files(woYAML(""))})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if res.Version != 1 || res.Status != StatusActive {
		t.Fatalf("v1 = %d %s", res.Version, res.Status)
	}
	var exists bool
	pool.QueryRow(ctx, `SELECT to_regclass('obj.work_order') IS NOT NULL`).Scan(&exists)
	if !exists {
		t.Error("obj.work_order table not created")
	}
	var ridCount int
	pool.QueryRow(ctx, `SELECT count(*) FROM oms.type_registry WHERE ontology='mro'`).Scan(&ridCount)
	if ridCount == 0 {
		t.Error("no RIDs registered")
	}

	// Idempotent re-publish of the same content -> reused version 1.
	res2, err := svc.Publish(ctx, PublishInput{Ontology: "mro", GitCommit: "c1b", Actor: "u", Files: files(woYAML(""))})
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if !res2.Reused || res2.Version != 1 {
		t.Errorf("re-publish = %+v, want reused v1", res2)
	}

	// Add an optional property -> version 2, ALTER TABLE applied, RID stable.
	var ridBefore string
	pool.QueryRow(ctx, `SELECT rid FROM oms.type_registry WHERE ontology='mro' AND api_name='WorkOrder'`).Scan(&ridBefore)
	res3, err := svc.Publish(ctx, PublishInput{Ontology: "mro", GitCommit: "c2", Actor: "u",
		Files: files(woYAML("    note: { type: string }\n"))})
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if res3.Version != 2 {
		t.Fatalf("v2 = %d", res3.Version)
	}
	pool.QueryRow(ctx, `SELECT to_regclass('obj.work_order') IS NOT NULL`).Scan(&exists)
	var hasNote bool
	pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='obj' AND table_name='work_order' AND column_name='note')`).Scan(&hasNote)
	if !hasNote {
		t.Error("note column not added by DDL")
	}
	var ridAfter string
	pool.QueryRow(ctx, `SELECT rid FROM oms.type_registry WHERE ontology='mro' AND api_name='WorkOrder'`).Scan(&ridAfter)
	if ridBefore == "" || ridBefore != ridAfter {
		t.Errorf("RID should be stable across versions: %q -> %q", ridBefore, ridAfter)
	}

	// Breaking change without migration -> PUB002, version stays 2.
	_, err = svc.Publish(ctx, PublishInput{Ontology: "mro", GitCommit: "c3", Actor: "u",
		Files: files(woYAML2DropStatus())})
	if err == nil {
		t.Fatal("expected PUB002 for dropped property")
	}
	active, _ := store.GetActiveCompiled(ctx, "mro")
	if active.Version != 2 {
		t.Errorf("active version after rejected publish = %d, want 2", active.Version)
	}

	// Rollback to v1 via activate.
	if err := svc.ActivateVersion(ctx, "mro", 1); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	active, _ = store.GetActiveCompiled(ctx, "mro")
	if active.Version != 1 {
		t.Errorf("active after rollback = %d, want 1", active.Version)
	}
}

// drops the status property (breaking) without a migration.
func woYAML2DropStatus() string {
	return `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
  datasource: { connector: mro_db, table: public.wo }
`
}

func TestPublishValidationFailure(t *testing.T) {
	ctx, _, svc, _ := setup(t)
	bad := `objectType:
  apiName: WorkOrder
  primaryKey: missing
  titleProperty: id
  properties:
    id: { type: string, required: true }
  datasource: { connector: mro_db, table: public.wo }
`
	_, err := svc.Publish(ctx, PublishInput{Ontology: "mro", GitCommit: "c", Actor: "u", Files: files(bad)})
	if err == nil {
		t.Fatal("expected validation failure")
	}
}
