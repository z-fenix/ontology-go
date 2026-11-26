//go:build integration

package action

import (
	"testing"
	"time"

	"github.com/acme/ontology/internal/objstore"
)

func TestIntegrationAuditQueries(t *testing.T) {
	ctx, pool, e, _ := integrationEngine(t)
	c := e.Committer.(*PGCommitter)
	first, err := e.Apply(ctx, assign("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	// A second run by another actor that touches no object (validation only writes nothing, so
	// insert it directly).
	if _, err := pool.Exec(ctx, `INSERT INTO edits.action_run (action_rid, ontology, ontology_ver, action_type,
  actor_id, actor_kind, on_behalf_of, request_hash, parameters, status, created_at)
VALUES ('ri.action.other', 'mro', 1, 'groundAircraft', 'agent-1', 'AGENT', 'bob', 'h', '{}', 'FAILED', now() + interval '1 second')`); err != nil {
		t.Fatal(err)
	}

	all, next, err := c.ListRuns(ctx, RunFilter{Ontology: "mro"})
	if err != nil || len(all) != 2 || next != nil || all[0].ActionRID != "ri.action.other" {
		t.Fatalf("all runs = %+v, next = %v, err = %v", all, next, err)
	}
	if all[0].OnBehalfOf != "bob" || all[0].ActorKind != "AGENT" || all[1].CommittedAt == nil {
		t.Errorf("summaries = %+v", all)
	}

	page1, next, err := c.ListRuns(ctx, RunFilter{Ontology: "mro", Limit: 1})
	if err != nil || len(page1) != 1 || next == nil {
		t.Fatalf("page 1 = %+v, %v, %v", page1, next, err)
	}
	page2, next2, err := c.ListRuns(ctx, RunFilter{Ontology: "mro", Limit: 1, After: next})
	if err != nil || len(page2) != 1 || next2 != nil || page2[0].ActionRID != first.ActionRID {
		t.Fatalf("page 2 = %+v, %v, %v", page2, next2, err)
	}

	byObject, _, err := c.ListRuns(ctx, RunFilter{Object: &objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"}})
	if err != nil || len(byObject) != 1 || byObject[0].ActionRID != first.ActionRID {
		t.Errorf("by object = %+v, %v", byObject, err)
	}
	for _, f := range []RunFilter{
		{ActorID: "agent-1"}, {Status: "FAILED"}, {ActionType: "groundAircraft"},
		{Since: time.Now().Add(500 * time.Millisecond)},
	} {
		got, _, err := c.ListRuns(ctx, f)
		if err != nil || len(got) != 1 || got[0].ActionRID != "ri.action.other" {
			t.Errorf("filter %+v = %+v, %v", f, got, err)
		}
	}

	edits, err := c.ListEdits(ctx, first.ActionRID)
	if err != nil || len(edits) != 1 || edits[0].Op != "MODIFY" || edits[0].Changes["status"] != "IN_PROGRESS" ||
		edits[0].Before["status"] != "OPEN" {
		t.Errorf("edits = %+v, %v", edits, err)
	}
	rr, err := c.GetRun(ctx, "ri.action.other")
	if err != nil || rr.OnBehalfOf != "bob" {
		t.Errorf("GetRun = %+v, %v", rr, err)
	}
}

func TestIntegrationStuckRuns(t *testing.T) {
	ctx, pool, e, _ := integrationEngine(t)
	c := e.Committer.(*PGCommitter)
	committed, err := e.Apply(ctx, assign("WO-1")) // leaves one PENDING notify side effect
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO edits.action_run (action_rid, ontology, ontology_ver, action_type,
  actor_id, actor_kind, request_hash, parameters, status)
VALUES ('ri.action.pending', 'mro', 1, 'assignWorkOrder', 'alice', 'USER', 'h', '{}', 'WRITEBACK_PENDING')`); err != nil {
		t.Fatal(err)
	}

	if runs, err := c.StuckRuns(ctx, time.Minute, 10); err != nil || len(runs) != 0 {
		t.Fatalf("fresh runs are not stuck: %+v, %v", runs, err)
	}
	for _, q := range []string{
		`UPDATE edits.action_run SET updated_at = now() - interval '10 minutes'`,
		`UPDATE edits.side_effect_run SET updated_at = now() - interval '10 minutes'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := c.StuckRuns(ctx, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range runs {
		got[r.ActionRID] = r.Kind
	}
	if len(got) != 2 || got["ri.action.pending"] != SweepWriteback || got[committed.ActionRID] != SweepSideEffects {
		t.Fatalf("stuck = %v", got)
	}

	for _, r := range runs {
		if err := c.TouchRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if runs, err := c.StuckRuns(ctx, time.Minute, 10); err != nil || len(runs) != 0 {
		t.Errorf("touched runs must wait another interval: %+v, %v", runs, err)
	}

	// Once the side effect has run, the committed run is no longer stuck.
	if _, err := pool.Exec(ctx, `UPDATE edits.side_effect_run SET status = 'SUCCEEDED', updated_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE edits.action_run SET updated_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	runs, err = c.StuckRuns(ctx, time.Minute, 10)
	if err != nil || len(runs) != 1 || runs[0].ActionRID != "ri.action.pending" {
		t.Errorf("after side effects succeeded: %+v, %v", runs, err)
	}
}
