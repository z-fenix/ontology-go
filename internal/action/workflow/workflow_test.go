package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/acme/ontology/internal/action"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
	"github.com/acme/ontology/internal/webhook"
)

const rid = "ri.action-run.test"

type memStore struct {
	mu        sync.Mutex
	plan      *action.Plan
	commitErr error
	committed bool
	status    string
	code      string
	effects   map[string]action.EffectAttempt // name|mode
}

func (m *memStore) LoadPlan(context.Context, string) (*action.Plan, error) { return m.plan, nil }

func (m *memStore) LoadEffects(context.Context, string) ([]action.PlannedEffect, error) {
	var out []action.PlannedEffect
	for _, ef := range m.plan.Effects {
		if ef.Mode == action.ModeSideEffect {
			out = append(out, ef)
		}
	}
	return out, nil
}

func (m *memStore) CommitReserved(_ context.Context, in *action.CommitInput) (*action.ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.commitErr != nil {
		return nil, m.commitErr
	}
	m.committed, m.status = true, action.StatusCommitted
	r := *in.Result
	r.ActionRID, r.Status = in.ActionRID, action.StatusCommitted
	return &r, nil
}

func (m *memStore) RecordEffect(_ context.Context, _ string, a action.EffectAttempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.effects == nil {
		m.effects = map[string]action.EffectAttempt{}
	}
	m.effects[a.Name+"|"+a.Mode] = a
	return nil
}

func (m *memStore) EffectSucceeded(_ context.Context, _, name, mode string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effects[name+"|"+mode].Status == "SUCCEEDED", nil
}

func (m *memStore) MarkRun(_ context.Context, _, status, code, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.code = status, code
	return nil
}

type fakeHooks struct {
	mu    sync.Mutex
	fail  map[string]error // URL -> error
	calls []string
}

func (f *fakeHooks) Do(_ context.Context, req webhook.Request, _ string) (*webhook.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.URL)
	if err := f.fail[req.URL]; err != nil {
		return nil, err
	}
	return &webhook.Response{Status: 200}, nil
}

type ontSource struct{}

func (ontSource) Active(context.Context) (*spec.CompiledOntology, error) {
	return &spec.CompiledOntology{Version: 1}, nil
}
func (ontSource) Get(context.Context, int64) (*spec.CompiledOntology, error) {
	return &spec.CompiledOntology{Version: 1}, nil
}

func plan(withCompensate bool) *action.Plan {
	wb := action.PlannedEffect{Kind: action.EffectWebhook, Name: "mro", Mode: action.ModeWriteback,
		Request: &webhook.Request{URL: "https://mro.internal/assign", Retry: spec.WebhookRetry{Max: 3}}}
	if withCompensate {
		wb.Compensate = &webhook.Request{URL: "https://mro.internal/unassign"}
	}
	return &action.Plan{OntologyVersion: 1, Result: action.ApplyResult{Status: action.StatusCommitted},
		Edits: []objstore.Edit{{Op: objstore.OpModify, Ref: objstore.ObjectRef{Type: "WorkOrder", PK: "WO-1"}}},
		Effects: []action.PlannedEffect{wb, {Kind: action.EffectWebhook, Name: "audit", Mode: action.ModeSideEffect,
			Request: &webhook.Request{URL: "https://audit.internal/log"}}}}
}

func run(t *testing.T, store *memStore, hooks *fakeHooks) action.WritebackOutcome {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ActionWorkflow)
	env.RegisterWorkflow(SideEffectsWorkflow)
	env.RegisterActivity(&Activities{Store: store, Ontologies: ontSource{}, Webhooks: hooks})
	env.ExecuteWorkflow(ActionWorkflow, Input{ActionRID: rid})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var out action.WritebackOutcome
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWritebackSuccessCommitsAndRunsSideEffects(t *testing.T) { // E07
	store, hooks := &memStore{plan: plan(true)}, &fakeHooks{}
	out := run(t, store, hooks)
	if out.Status != action.StatusCommitted || !store.committed {
		t.Fatalf("outcome = %+v committed=%v", out, store.committed)
	}
	if len(hooks.calls) != 2 || hooks.calls[0] != "https://mro.internal/assign" || hooks.calls[1] != "https://audit.internal/log" {
		t.Errorf("calls = %v", hooks.calls)
	}
	if store.effects["mro|WRITEBACK"].Status != "SUCCEEDED" {
		t.Error("write-back attempt not recorded")
	}
}

func TestWritebackFailureLeavesOntologyUntouched(t *testing.T) { // E09
	hooks := &fakeHooks{fail: map[string]error{"https://mro.internal/assign": &webhook.Error{Status: 500, Retryable: true, Msg: "down"}}}
	store := &memStore{plan: plan(true)}
	out := run(t, store, hooks)
	if out.Status != action.StatusFailed || out.ErrorCode != ErrWritebackFailed || store.committed {
		t.Fatalf("outcome = %+v committed=%v", out, store.committed)
	}
	if n := len(hooks.calls); n != 3 { // retry.max = 3 attempts
		t.Errorf("write-back attempts = %d, want 3", n)
	}
}

func TestPermanentWritebackErrorIsNotRetried(t *testing.T) {
	hooks := &fakeHooks{fail: map[string]error{"https://mro.internal/assign": &webhook.Error{Status: 400, Msg: "bad"}}}
	out := run(t, &memStore{plan: plan(true)}, hooks)
	if out.Status != action.StatusFailed || len(hooks.calls) != 1 {
		t.Fatalf("outcome = %+v calls=%d", out, len(hooks.calls))
	}
}

func TestCommitConflictCompensates(t *testing.T) { // E10
	store := &memStore{plan: plan(true), commitErr: &objstore.EditError{Err: objstore.ErrStale}}
	hooks := &fakeHooks{}
	out := run(t, store, hooks)
	if out.Status != action.StatusCompensated || out.ErrorCode != "STALE_OBJECT" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(hooks.calls) != 2 || hooks.calls[1] != "https://mro.internal/unassign" {
		t.Errorf("calls = %v (want write-back then compensate, no side effects)", hooks.calls)
	}
	if store.status != action.StatusCompensated {
		t.Errorf("run status = %s", store.status)
	}
}

func TestCommitConflictWithoutCompensateIsInconsistent(t *testing.T) {
	store := &memStore{plan: plan(false), commitErr: &objstore.EditError{Err: objstore.ErrStale}}
	out := run(t, store, &fakeHooks{})
	if out.Status != action.StatusFailed || out.ErrorCode != "WRITEBACK_INCONSISTENT" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestErrorCodeExtraction(t *testing.T) {
	err := temporal.NewNonRetryableApplicationError("x", "STALE_OBJECT", errors.New("y"))
	if c := errorCode(err, ErrCommitFailed); c != "STALE_OBJECT" {
		t.Errorf("code = %s", c)
	}
	if c := errorCode(errors.New("plain"), ErrCommitFailed); c != ErrCommitFailed {
		t.Errorf("code = %s", c)
	}
}
