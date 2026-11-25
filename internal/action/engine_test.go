package action

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"

	_ "github.com/acme/ontology/internal/expr" // CEL validators for spec.Compile
)

func loadOntology(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if diags := append(ld, cd...); diags.HasErrors(false) {
		t.Fatalf("example ontology: %v", diags.Codes())
	}
	ont.Version = 1
	// The example assignWorkOrder has a WRITEBACK webhook; tests exercise the commit path on a copy without it.
	a := *ont.ActionTypes["assignWorkOrder"]
	a.APIName = "assignLocal"
	a.SideEffects = []spec.SideEffect{{NotifyTo: "{{technician.email}}", Template: "wo_assigned"}}
	ont.ActionTypes["assignLocal"] = &a
	return ont
}

type fakeOnt struct{ o *spec.CompiledOntology }

func (f fakeOnt) Active(context.Context) (*spec.CompiledOntology, error) { return f.o, nil }
func (f fakeOnt) Get(_ context.Context, v int64) (*spec.CompiledOntology, error) {
	if v != f.o.Version {
		return nil, errors.New("no such version")
	}
	return f.o, nil
}

type fakeObjects struct{ objs map[objstore.ObjectRef]*objstore.Object }

func (f *fakeObjects) GetMany(_ context.Context, _ *spec.CompiledOntology, refs []objstore.ObjectRef) (map[objstore.ObjectRef]*objstore.Object, error) {
	out := map[objstore.ObjectRef]*objstore.Object{}
	for _, r := range refs {
		if o, ok := f.objs[r]; ok {
			out[r] = o
		}
	}
	return out, nil
}

func (f *fakeObjects) Find(_ context.Context, _ *spec.CompiledOntology, typ string, flt objstore.Filter, _ objstore.Page) ([]*objstore.Object, string, error) {
	var out []*objstore.Object
	for ref, o := range f.objs {
		if ref.Type != typ {
			continue
		}
		match := true
		for k, v := range flt.Eq {
			if o.Props[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, o)
		}
	}
	return out, "", nil
}

type fakeAuthz struct {
	deny map[string]bool // "user|relation|object"
	err  error
}

func (f fakeAuthz) Check(_ context.Context, user, rel, obj string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return !f.deny[user+"|"+rel+"|"+obj], nil
}

type fakeCommitter struct {
	mu       sync.Mutex
	commits  []*CommitInput
	reserved []*CommitInput
	marks    []string
	byKey   map[string]*PriorRun
	err     error
}

func (f *fakeCommitter) FindByIdempotency(_ context.Context, actor, at, key string) (*PriorRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byKey[actor+"|"+at+"|"+key], nil
}

func (f *fakeCommitter) Commit(_ context.Context, in *CommitInput) (*ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.commits = append(f.commits, in)
	res := *in.Result
	res.ActionRID = in.ActionRID
	res.ConsistencyToken = "edit:1"
	if in.IdempotencyKey != "" {
		if f.byKey == nil {
			f.byKey = map[string]*PriorRun{}
		}
		f.byKey[in.Actor.ID+"|"+in.ActionType+"|"+in.IdempotencyKey] = &PriorRun{ActionRID: in.ActionRID,
			RequestHash: in.RequestHash, Status: StatusCommitted, Result: &res}
	}
	return &res, nil
}

func (f *fakeCommitter) Reserve(_ context.Context, in *CommitInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved = append(f.reserved, in)
	return nil
}

func (f *fakeCommitter) MarkRun(_ context.Context, rid, status, code, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, rid+"|"+status+"|"+code)
	return nil
}

func (f *fakeCommitter) GetRun(context.Context, string) (*RunRecord, error) { return nil, nil }

type fakeDispatcher struct {
	outcome    *WritebackOutcome
	err        error
	sideEffect []string
}

func (d *fakeDispatcher) RunWriteback(context.Context, string, time.Duration) (*WritebackOutcome, error) {
	return d.outcome, d.err
}

func (d *fakeDispatcher) StartSideEffects(_ context.Context, rid string) error {
	d.sideEffect = append(d.sideEffect, rid)
	return nil
}

func obj(typ, pk string, props map[string]any) *objstore.Object {
	return &objstore.Object{Ref: objstore.ObjectRef{Type: typ, PK: pk}, Version: 1, Props: props}
}

func fixture(t *testing.T) (*Engine, *fakeCommitter, *fakeObjects) {
	ont := loadOntology(t)
	objs := &fakeObjects{objs: map[objstore.ObjectRef]*objstore.Object{}}
	for _, o := range []*objstore.Object{
		obj("WorkOrder", "WO-1", map[string]any{"workOrderId": "WO-1", "summary": "s", "status": "OPEN", "tailNumber": "B-1234", "priority": int64(3)}),
		obj("WorkOrder", "WO-DONE", map[string]any{"workOrderId": "WO-DONE", "summary": "s", "status": "DONE", "tailNumber": "B-1234"}),
		obj("Technician", "T-1", map[string]any{"technicianId": "T-1", "name": "Lee", "email": "lee@x"}),
		obj("Aircraft", "B-1234", map[string]any{"tailNumber": "B-1234", "status": "ACTIVE"}),
	} {
		objs.objs[o.Ref] = o
	}
	c := &fakeCommitter{}
	reg := NewFunctionRegistry()
	return &Engine{Ontologies: fakeOnt{ont}, Objects: objs, Authz: fakeAuthz{}, Committer: c, Functions: reg}, c, objs
}

func code(t *testing.T, err error) string {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperr.Error, got %T: %v", err, err)
	}
	return ae.Code
}

var user = Actor{ID: "alice", Kind: ActorUser}

func assign(wo string) ApplyRequest {
	return ApplyRequest{ActionType: "assignLocal", Actor: user, Mode: ModeApply,
		Parameters: map[string]any{"workOrder": wo, "technician": "T-1", "note": "night shift"}}
}

func TestApplyCommitsRuleEdits(t *testing.T) {
	e, c, _ := fixture(t)
	res, err := e.Apply(context.Background(), assign("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCommitted || len(res.Edits.Modified) != 1 || res.Edits.Modified[0].PK != "WO-1" {
		t.Fatalf("result = %+v", res)
	}
	ed := c.commits[0].Edits[0]
	if ed.Op != objstore.OpModify || ed.ExpectedVersion != 1 {
		t.Errorf("edit = %+v", ed)
	}
	if ed.Changes["status"] != "IN_PROGRESS" || ed.Changes["technicianId"] != "T-1" {
		t.Errorf("changes = %v", ed.Changes)
	}
	if c.commits[0].Before[ed.Ref]["status"] != "OPEN" {
		t.Error("before snapshot missing")
	}
}

func TestSubmissionCriteriaFail(t *testing.T) { // E08
	e, c, _ := fixture(t)
	_, err := e.Apply(context.Background(), assign("WO-DONE"))
	if code(t, err) != "SUBMISSION_CRITERIA_FAILED" {
		t.Fatalf("code = %s", code(t, err))
	}
	var ae *apperr.Error
	errors.As(err, &ae)
	if ae.HTTPStatus != http.StatusUnprocessableEntity || !strings.Contains(ae.Message, "OPEN") {
		t.Errorf("error = %+v", ae)
	}
	if len(c.commits) != 0 {
		t.Error("nothing must be committed")
	}
}

func TestValidateOnlyWritesNothing(t *testing.T) { // E15
	e, c, _ := fixture(t)
	req := assign("WO-1")
	req.Mode = ModeValidate
	res, err := e.Apply(context.Background(), req)
	if err != nil || res.Status != StatusValid || len(res.Validation.Criteria) != 2 {
		t.Fatalf("validate = %+v, %v", res, err)
	}
	if len(c.commits) != 0 {
		t.Error("validate must not commit")
	}
}

func TestParameterErrorsCollected(t *testing.T) {
	e, _, _ := fixture(t)
	_, err := e.Apply(context.Background(), ApplyRequest{ActionType: "assignLocal", Actor: user,
		Parameters: map[string]any{"bogus": 1, "note": 5}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	if n := len(ae.Details["problems"].([]paramProblem)); n < 4 { // unknown, 2x missing, invalid note
		t.Errorf("problems = %d, want >= 4", n)
	}
}

func TestMissingObject(t *testing.T) {
	e, _, _ := fixture(t)
	if c := code(t, func() error { _, err := e.Apply(context.Background(), assign("WO-404")); return err }()); c != "OBJECT_NOT_FOUND" {
		t.Errorf("code = %s", c)
	}
}

func TestPermissionDenied(t *testing.T) { // E12
	e, _, _ := fixture(t)
	e.Authz = fakeAuthz{deny: map[string]bool{"user:alice|can_assign|work_order:WO-1": true}}
	_, err := e.Apply(context.Background(), assign("WO-1"))
	if code(t, err) != "PERMISSION_DENIED" {
		t.Fatalf("code = %s", code(t, err))
	}
}

func TestInvisibleObjectLooksMissing(t *testing.T) {
	e, _, _ := fixture(t)
	e.Authz = fakeAuthz{deny: map[string]bool{"user:alice|viewer|work_order:WO-1": true}}
	_, err := e.Apply(context.Background(), assign("WO-1"))
	if code(t, err) != "OBJECT_NOT_FOUND" {
		t.Fatalf("code = %s", code(t, err))
	}
}

func TestAgentNeedsOnBehalfPermission(t *testing.T) { // E13
	e, _, _ := fixture(t)
	e.Authz = fakeAuthz{deny: map[string]bool{"user:bob|can_assign|work_order:WO-1": true}}
	req := assign("WO-1")
	req.Actor = Actor{ID: "agent-1", Kind: ActorAgent, OnBehalfOf: "bob"}
	if c := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); c != "PERMISSION_DENIED" {
		t.Fatalf("code = %s", c)
	}
	req.Actor.OnBehalfOf = ""
	if c := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); c != "OBJECT_NOT_FOUND" {
		t.Fatalf("agent without on-behalf-of: code = %s", c)
	}
}

func TestAuthzOutageFailsClosed(t *testing.T) {
	e, _, _ := fixture(t)
	e.Authz = fakeAuthz{err: errors.New("down")}
	_, err := e.Apply(context.Background(), assign("WO-1"))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestIdempotencyReplayAndReuse(t *testing.T) { // E11
	e, c, _ := fixture(t)
	req := assign("WO-1")
	req.IdempotencyKey = "k1"
	r1, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.Apply(context.Background(), req)
	if err != nil || r2.ActionRID != r1.ActionRID {
		t.Fatalf("replay = %+v, %v", r2, err)
	}
	if len(c.commits) != 1 {
		t.Errorf("commits = %d, want 1", len(c.commits))
	}
	req.Parameters = map[string]any{"workOrder": "WO-1", "technician": "T-1", "note": "different"}
	if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("code = %s", cd)
	}
}

func TestWritebackWithoutDispatcherRejected(t *testing.T) {
	e, c, _ := fixture(t)
	req := assign("WO-1")
	req.ActionType = "assignWorkOrder"
	if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != "WRITEBACK_NOT_SUPPORTED" {
		t.Fatalf("code = %s", cd)
	}
	if len(c.commits) != 0 || len(c.reserved) != 0 {
		t.Error("nothing must be written")
	}
}

func TestWritebackReservesAndRendersEffects(t *testing.T) {
	e, c, _ := fixture(t)
	d := &fakeDispatcher{} // nil outcome = still running
	e.Dispatcher = d
	req := assign("WO-1")
	req.ActionType = "assignWorkOrder"
	res, err := e.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusWritebackPending || len(c.reserved) != 1 || len(c.commits) != 0 {
		t.Fatalf("status=%s reserved=%d commits=%d", res.Status, len(c.reserved), len(c.commits))
	}
	in := c.reserved[0]
	if len(in.Effects) != 2 {
		t.Fatalf("effects = %+v", in.Effects)
	}
	wb := in.Effects[0]
	if wb.Mode != ModeWriteback || wb.Request.URL != "https://mro.internal/api/work-orders/WO-1/assign" {
		t.Errorf("write-back request = %+v", wb.Request)
	}
	if wb.Request.Body["technician"] != "T-1" || wb.Request.Body["note"] != "night shift" {
		t.Errorf("write-back body = %v", wb.Request.Body)
	}
	if wb.Compensate == nil || wb.Compensate.URL != "https://mro.internal/api/work-orders/WO-1/unassign" {
		t.Errorf("compensate = %+v", wb.Compensate)
	}
	if n := in.Effects[1]; n.Kind != EffectNotify || n.NotifyTo != "lee@x" {
		t.Errorf("notify effect = %+v", n)
	}
}

func TestWritebackOutcomes(t *testing.T) { // E09 / E10 mapping
	cases := []struct {
		out  WritebackOutcome
		code string
	}{
		{WritebackOutcome{Status: StatusFailed, ErrorCode: "WRITEBACK_FAILED", Message: "MRO down"}, "WRITEBACK_FAILED"},
		{WritebackOutcome{Status: StatusCompensated, ErrorCode: "STALE_OBJECT", Message: "changed"}, "STALE_OBJECT"},
	}
	for _, cse := range cases {
		e, _, _ := fixture(t)
		out := cse.out
		e.Dispatcher = &fakeDispatcher{outcome: &out}
		req := assign("WO-1")
		req.ActionType = "assignWorkOrder"
		if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != cse.code {
			t.Errorf("%s: code = %s, want %s", cse.out.Status, cd, cse.code)
		}
	}
}

func TestWorkflowStartFailureMarksRun(t *testing.T) {
	e, c, _ := fixture(t)
	e.Dispatcher = &fakeDispatcher{err: errors.New("temporal down")}
	req := assign("WO-1")
	req.ActionType = "assignWorkOrder"
	if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != "WORKFLOW_UNAVAILABLE" {
		t.Fatalf("code = %s", cd)
	}
	if len(c.marks) != 1 || !strings.Contains(c.marks[0], "|FAILED|WORKFLOW_START_FAILED") {
		t.Errorf("marks = %v", c.marks)
	}
}

func TestSideEffectsDispatchedAfterCommit(t *testing.T) {
	e, _, _ := fixture(t)
	d := &fakeDispatcher{}
	e.Dispatcher = d
	res, err := e.Apply(context.Background(), assign("WO-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.sideEffect) != 1 || d.sideEffect[0] != res.ActionRID {
		t.Errorf("side effects dispatched = %v", d.sideEffect)
	}
}

func TestStaleRetriedThenReported(t *testing.T) {
	e, c, _ := fixture(t)
	c.err = &objstore.EditError{Index: 0, Err: objstore.ErrStale}
	_, err := e.Apply(context.Background(), assign("WO-1"))
	if code(t, err) != "STALE_OBJECT" {
		t.Fatalf("code = %s", code(t, err))
	}
}

func TestFunctionBackedAction(t *testing.T) { // E14 (engine side)
	e, c, objs := fixture(t)
	e.Functions.Register("groundAircraft", func(ctx context.Context, in FuncInput) ([]objstore.Edit, error) {
		ac := in.Objects["aircraft"]
		open, err := in.Reader.Find(ctx, "WorkOrder", objstore.Filter{Eq: map[string]any{"tailNumber": ac.Ref.PK, "status": "OPEN"}}, 100)
		if err != nil {
			return nil, err
		}
		edits := []objstore.Edit{{Op: objstore.OpModify, Ref: ac.Ref, ExpectedVersion: ac.Version, Changes: map[string]any{"status": "GROUNDED"}}}
		for _, wo := range open {
			edits = append(edits, objstore.Edit{Op: objstore.OpModify, Ref: wo.Ref, ExpectedVersion: wo.Version, Changes: map[string]any{"priority": int64(1)}})
		}
		return edits, nil
	})
	_ = objs
	res, err := e.Apply(context.Background(), ApplyRequest{ActionType: "groundAircraft", Actor: user, Mode: ModeApply,
		Parameters: map[string]any{"aircraft": "B-1234", "reason": "bird strike"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edits.Modified) != 2 || len(c.commits[0].Edits) != 2 {
		t.Fatalf("result = %+v", res.Edits)
	}
}

func TestEditValidation(t *testing.T) {
	e, _, _ := fixture(t)
	e.Functions.Register("groundAircraft", func(_ context.Context, in FuncInput) ([]objstore.Edit, error) {
		return []objstore.Edit{{Op: objstore.OpModify, Ref: in.Objects["aircraft"].Ref, Changes: map[string]any{"status": "FLYING"}}}, nil
	})
	_, err := e.Apply(context.Background(), ApplyRequest{ActionType: "groundAircraft", Actor: user, Mode: ModeApply,
		Parameters: map[string]any{"aircraft": "B-1234", "reason": "x"}})
	if code(t, err) != "EDIT_VALIDATION_FAILED" { // status enum is ACTIVE|GROUNDED
		t.Fatalf("code = %s", code(t, err))
	}
}

func TestCriteriaFollowLinks(t *testing.T) { // CEL multi-hop: workOrder.aircraft.status
	e, _, objs := fixture(t)
	ont := e.Ontologies.(fakeOnt).o
	a := *ont.ActionTypes["assignLocal"]
	a.APIName = "assignActiveOnly"
	a.SubmissionCriteria = []spec.Criterion{{Expr: `workOrder.aircraft.status == "ACTIVE"`, Message: "aircraft is not active"}}
	ont.ActionTypes["assignActiveOnly"] = &a
	req := assign("WO-1")
	req.ActionType = "assignActiveOnly"
	if _, err := e.Apply(context.Background(), req); err != nil {
		t.Fatalf("active aircraft: %v", err)
	}
	objs.objs[objstore.ObjectRef{Type: "Aircraft", PK: "B-1234"}].Props["status"] = "GROUNDED"
	if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != "SUBMISSION_CRITERIA_FAILED" {
		t.Fatalf("grounded aircraft: code = %s", cd)
	}
	// An aircraft the user may not see behaves like no aircraft: the comparison fails.
	objs.objs[objstore.ObjectRef{Type: "Aircraft", PK: "B-1234"}].Props["status"] = "ACTIVE"
	e.Authz = fakeAuthz{deny: map[string]bool{"user:alice|viewer|aircraft:B-1234": true}}
	if cd := code(t, func() error { _, err := e.Apply(context.Background(), req); return err }()); cd != "SUBMISSION_CRITERIA_FAILED" {
		t.Fatalf("invisible aircraft: code = %s", cd)
	}
}
