//go:build integration

package action

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
)

// Requires ONTO_TEST_DSN with migrations applied (make dev-up). Resets obj and edits data.
func integrationEngine(t *testing.T) (context.Context, *pgxpool.Pool, *Engine, *objstore.Store) {
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
	stmts, err := objstore.GenerateDDL(ont)
	if err != nil {
		t.Fatal(err)
	}
	reset := append([]string{`DROP SCHEMA IF EXISTS obj CASCADE`}, stmts...)
	reset = append(reset, `TRUNCATE edits.side_effect_run, edits.outbox, edits.object_edit, edits.action_run`)
	for _, s := range reset {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	store := objstore.New(pool)
	seed := []objstore.Edit{
		{Op: objstore.OpCreate, Ref: objstore.ObjectRef{Type: "Technician", PK: "T-1"}, Changes: map[string]any{"name": "Lee", "email": "lee@x"}},
		{Op: objstore.OpCreate, Ref: objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"},
			Changes: map[string]any{"summary": "check", "status": "OPEN", "tailNumber": "B-1234", "priority": 3}},
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { _, err := store.ApplyEdits(ctx, tx, ont, seed); return err }); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Ontologies: fakeOnt{ont}, Objects: store, Authz: authz.AllowAll{},
		Committer: NewPGCommitter(pool, store), Functions: NewFunctionRegistry()}
	return ctx, pool, e, store
}

func TestIntegrationCommitAndAudit(t *testing.T) { // E07 without write-back
	ctx, pool, e, store := integrationEngine(t)
	req := assign("WO-1")
	req.IdempotencyKey = "idem-1"
	res, err := e.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	o, err := store.Get(ctx, loadOntology(t), objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"})
	if err != nil || o.Props["status"] != "IN_PROGRESS" || o.Props["technicianId"] != "T-1" || o.Version != 2 {
		t.Fatalf("object after apply = %+v, %v", o, err)
	}
	var edits, outbox, effects int
	pool.QueryRow(ctx, `SELECT count(*) FROM edits.object_edit WHERE action_rid=$1`, res.ActionRID).Scan(&edits)
	pool.QueryRow(ctx, `SELECT count(*) FROM edits.outbox`).Scan(&outbox)
	pool.QueryRow(ctx, `SELECT count(*) FROM edits.side_effect_run WHERE action_rid=$1 AND status='PENDING'`, res.ActionRID).Scan(&effects)
	if edits != 1 || outbox != 2 || effects != 1 {
		t.Errorf("edits=%d outbox=%d pending side effects=%d; want 1, 2, 1", edits, outbox, effects)
	}
	if res.ConsistencyToken == "" {
		t.Error("missing consistency token")
	}

	// Same key again: replayed, no second write.
	again, err := e.Apply(ctx, req)
	if err != nil || again.ActionRID != res.ActionRID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	var runs int
	pool.QueryRow(ctx, `SELECT count(*) FROM edits.action_run`).Scan(&runs)
	if runs != 1 {
		t.Errorf("action runs = %d, want 1", runs)
	}
}

func TestIntegrationConcurrentIdempotency(t *testing.T) {
	ctx, pool, e, _ := integrationEngine(t)
	var wg sync.WaitGroup
	rids := make([]string, 10)
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := assign("WO-1")
			req.IdempotencyKey = "race"
			r, err := e.Apply(ctx, req)
			errs[i] = err
			if r != nil {
				rids[i] = r.ActionRID
			}
		}(i)
	}
	wg.Wait()
	var runs int
	pool.QueryRow(ctx, `SELECT count(*) FROM edits.action_run WHERE idempotency_key='race'`).Scan(&runs)
	if runs != 1 {
		t.Fatalf("committed runs = %d, want exactly 1", runs)
	}
	for i, err := range errs {
		var ae *apperr.Error
		// Losers either replay the winner or see the object already changed (criteria / stale).
		if err != nil && !(errors.As(err, &ae) && (ae.Code == "SUBMISSION_CRITERIA_FAILED" || ae.Code == "STALE_OBJECT")) {
			t.Errorf("request %d: unexpected error %v", i, err)
		}
	}
}
