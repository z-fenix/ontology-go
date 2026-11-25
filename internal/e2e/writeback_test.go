//go:build integration

// Package e2e runs the write-back path end to end: the real action engine and PostgreSQL
// committer, a real Temporal server and worker, the real webhook executor, and a mock MRO
// system over HTTP. It covers acceptance cases E07 (write-back then commit), E09 (write-back
// fails, nothing changes), E10 (commit fails after write-back, compensation runs) and E11
// (idempotent replay calls the external system once).
//
// Requirements: ONTO_TEST_DSN (migrations applied). Temporal comes from ONTO_TEST_TEMPORAL
// (host:port of a running server, e.g. localhost:7233 from `make dev-up`); otherwise the SDK
// starts a dev server, downloading the Temporal CLI on first use (or TEMPORAL_CLI_PATH).
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/acme/ontology/internal/action"
	"github.com/acme/ontology/internal/action/workflow"
	"github.com/acme/ontology/internal/authz"
	_ "github.com/acme/ontology/internal/expr"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
	"github.com/acme/ontology/internal/webhook"
)

type ontSource struct{ o *spec.CompiledOntology }

func (s ontSource) Active(context.Context) (*spec.CompiledOntology, error) { return s.o, nil }
func (s ontSource) Get(_ context.Context, v int64) (*spec.CompiledOntology, error) {
	if v != s.o.Version {
		return nil, errors.New("unknown ontology version")
	}
	return s.o, nil
}

// mro is the mock external maintenance system.
type mro struct {
	mu       sync.Mutex
	mode     string // ok | fail | stale
	requests []mroRequest
	onStale  func() // changes the work order while the write-back is in flight
}

type mroRequest struct {
	Path, IdempotencyKey string
	Body                 map[string]any
}

func (m *mro) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	m.mu.Lock()
	m.requests = append(m.requests, mroRequest{r.URL.Path, r.Header.Get("Idempotency-Key"), body})
	mode, onStale := m.mode, m.onStale
	m.mu.Unlock()
	if mode == "fail" && strings.HasSuffix(r.URL.Path, "/assign") {
		http.Error(w, `{"error":"MRO is down"}`, http.StatusInternalServerError)
		return
	}
	if mode == "stale" && strings.HasSuffix(r.URL.Path, "/assign") && onStale != nil {
		onStale()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (m *mro) reset(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode, m.requests = mode, nil
}

func (m *mro) snapshot() []mroRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mroRequest(nil), m.requests...)
}

func (m *mro) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.requests))
	for i, r := range m.requests {
		out[i] = r.Path
	}
	return out
}

type notifier struct {
	mu   sync.Mutex
	sent map[string]string // actionRid -> recipient
}

func (n *notifier) Notify(_ context.Context, to, _ string, data map[string]any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	rid, _ := data["actionRid"].(string)
	n.sent[rid] = to
	return nil
}

func (n *notifier) to(rid string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sent[rid]
}

type env struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	ont    *spec.CompiledOntology
	store  *objstore.Store
	engine *action.Engine
	mro    *mro
	notify *notifier
}

func temporalClient(t *testing.T) client.Client {
	t.Helper()
	if hp := os.Getenv("ONTO_TEST_TEMPORAL"); hp != "" {
		c, err := client.Dial(client.Options{HostPort: hp})
		if err != nil {
			t.Fatalf("dial temporal %s: %v", hp, err)
		}
		t.Cleanup(c.Close)
		return c
	}
	srv, err := testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{
		ClientOptions: &client.Options{},
		ExistingPath:  os.Getenv("TEMPORAL_CLI_PATH"),
		LogLevel:      "warn",
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv.Client()
}

func setup(t *testing.T) *env {
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

	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example ontology: %v", d.Codes())
	}
	ont.Version = 1

	m := &mro{mode: "ok"}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	wh := ont.Webhooks["mro_assign_writeback"]
	wh.URL = strings.Replace(wh.URL, "https://mro.internal", srv.URL, 1)
	wh.Compensate.URL = strings.Replace(wh.Compensate.URL, "https://mro.internal", srv.URL, 1)
	wh.Auth = spec.WebhookAuth{Type: "none"}

	ddl, err := objstore.GenerateDDL(ont)
	if err != nil {
		t.Fatal(err)
	}
	reset := append([]string{`DROP SCHEMA IF EXISTS obj CASCADE`}, ddl...)
	reset = append(reset, `TRUNCATE edits.side_effect_run, edits.outbox, edits.object_edit, edits.action_run`)
	for _, s := range reset {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	store := objstore.New(pool)
	committer := action.NewPGCommitter(pool, store)
	src := ontSource{ont}
	n := &notifier{sent: map[string]string{}}

	tc := temporalClient(t)
	w := worker.New(tc, workflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflow.ActionWorkflow)
	w.RegisterWorkflow(workflow.SideEffectsWorkflow)
	w.RegisterActivity(&workflow.Activities{Store: committer, Ontologies: src,
		Webhooks: webhook.NewExecutor(webhook.EnvSecrets{}, []string{"127.0.0.1"}), Notifier: n})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)

	e := &env{ctx: ctx, pool: pool, ont: ont, store: store, mro: m, notify: n,
		engine: &action.Engine{Ontologies: src, Objects: store, Authz: authz.AllowAll{}, Committer: committer,
			Functions: action.NewFunctionRegistry(), Dispatcher: &workflow.Dispatcher{Client: tc}, SyncWait: 45 * time.Second}}
	m.onStale = func() {
		e.edit(objstore.Edit{Op: objstore.OpModify, Ref: wo("WO-1"), Changes: map[string]any{"priority": int64(1)}})
	}
	return e
}

func wo(pk string) objstore.ObjectRef { return objstore.ObjectRef{Type: "WorkOrder", PK: pk} }

func (e *env) edit(edits ...objstore.Edit) {
	if err := pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.store.ApplyEdits(e.ctx, tx, e.ont, edits)
		return err
	}); err != nil {
		panic(err)
	}
}

// seed resets the two objects every case starts from.
func (e *env) seed(t *testing.T) {
	t.Helper()
	for _, s := range []string{`DELETE FROM obj.work_order`, `DELETE FROM obj.technician`,
		`TRUNCATE edits.side_effect_run, edits.outbox, edits.object_edit, edits.action_run`} {
		if _, err := e.pool.Exec(e.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.edit(
		objstore.Edit{Op: objstore.OpCreate, Ref: objstore.ObjectRef{Type: "Technician", PK: "T-1"},
			Changes: map[string]any{"name": "Lee", "email": "lee@example.com"}},
		objstore.Edit{Op: objstore.OpCreate, Ref: wo("WO-1"),
			Changes: map[string]any{"summary": "A-check", "status": "OPEN", "tailNumber": "B-1234", "priority": int64(3)}},
	)
}

func (e *env) assign(key string) (*action.ApplyResult, error) {
	return e.engine.Apply(e.ctx, action.ApplyRequest{ActionType: "assignWorkOrder", IdempotencyKey: key, Mode: action.ModeApply,
		Actor:      action.Actor{ID: "alice", Kind: action.ActorUser},
		Parameters: map[string]any{"workOrder": "WO-1", "technician": "T-1", "note": "night shift"}})
}

func (e *env) workOrder(t *testing.T) *objstore.Object {
	t.Helper()
	o, err := e.store.Get(e.ctx, e.ont, wo("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) runStatus(t *testing.T, rid string) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM edits.action_run WHERE action_rid = $1`, rid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func errCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected an API error, got %v", err)
	}
	rid, _ := ae.Details["actionRid"].(string)
	return ae.Code, rid
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWritebackEndToEnd(t *testing.T) {
	e := setup(t)

	t.Run("E07 write-back succeeds, edits commit, side effects run", func(t *testing.T) {
		e.seed(t)
		e.mro.reset("ok")
		res, err := e.assign("")
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != action.StatusCommitted || res.ActionRID == "" {
			t.Fatalf("result = %+v", res)
		}
		o := e.workOrder(t)
		if o.Props["status"] != "IN_PROGRESS" || o.Props["technicianId"] != "T-1" {
			t.Errorf("work order = %v", o.Props)
		}
		reqs := e.mro.snapshot()
		if len(reqs) != 1 || reqs[0].Path != "/api/work-orders/WO-1/assign" {
			t.Fatalf("MRO requests = %v", e.mro.paths())
		}
		if reqs[0].Body["technician"] != "T-1" || reqs[0].Body["note"] != "night shift" ||
			reqs[0].IdempotencyKey != res.ActionRID+":mro_assign_writeback:WRITEBACK" {
			t.Errorf("write-back request = %+v", reqs[0])
		}
		eventually(t, "the notification side effect", func() bool { return e.notify.to(res.ActionRID) == "lee@example.com" })
		var status string
		eventually(t, "side_effect_run SUCCEEDED", func() bool {
			_ = e.pool.QueryRow(e.ctx, `SELECT status FROM edits.side_effect_run WHERE action_rid=$1 AND kind='notify'`, res.ActionRID).Scan(&status)
			return status == "SUCCEEDED"
		})
	})

	t.Run("E11 idempotent replay calls MRO once", func(t *testing.T) {
		e.seed(t)
		e.mro.reset("ok")
		first, err := e.assign("assign-WO-1")
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.assign("assign-WO-1")
		if err != nil || again.ActionRID != first.ActionRID {
			t.Fatalf("replay = %+v, %v", again, err)
		}
		if p := e.mro.paths(); len(p) != 1 {
			t.Errorf("MRO called %d times, want 1: %v", len(p), p)
		}
	})

	t.Run("E09 write-back fails, ontology unchanged", func(t *testing.T) {
		e.seed(t)
		e.mro.reset("fail")
		before := e.workOrder(t)
		_, err := e.assign("")
		code, rid := errCode(t, err)
		if code != "WRITEBACK_FAILED" {
			t.Fatalf("code = %s", code)
		}
		if p := e.mro.paths(); len(p) != 3 {
			t.Errorf("write-back attempts = %d, want 3 (retry.max): %v", len(p), p)
		}
		after := e.workOrder(t)
		if after.Version != before.Version || after.Props["status"] != "OPEN" || after.Props["technicianId"] != nil {
			t.Errorf("work order changed: %v (v%d -> v%d)", after.Props, before.Version, after.Version)
		}
		if s := e.runStatus(t, rid); s != action.StatusFailed {
			t.Errorf("run status = %s", s)
		}
		if e.notify.to(rid) != "" {
			t.Error("side effects must not run for a failed action")
		}
	})

	t.Run("E10 commit conflict after write-back is compensated", func(t *testing.T) {
		e.seed(t)
		e.mro.reset("stale")
		_, err := e.assign("")
		code, rid := errCode(t, err)
		if code != "STALE_OBJECT" {
			t.Fatalf("code = %s", code)
		}
		p := e.mro.paths()
		if len(p) != 2 || p[0] != "/api/work-orders/WO-1/assign" || p[1] != "/api/work-orders/WO-1/unassign" {
			t.Fatalf("MRO requests = %v, want assign then unassign", p)
		}
		if s := e.runStatus(t, rid); s != action.StatusCompensated {
			t.Errorf("run status = %s", s)
		}
		o := e.workOrder(t)
		if o.Props["technicianId"] != nil || o.Props["status"] != "OPEN" || o.Props["priority"] != int64(1) {
			t.Errorf("work order = %v (only the concurrent priority change may remain)", o.Props)
		}
		time.Sleep(time.Second)
		if e.notify.to(rid) != "" {
			t.Error("side effects must not run for a compensated action")
		}
	})
}
