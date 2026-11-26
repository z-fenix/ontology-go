package action

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/expr"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

// MaxEditedObjects bounds the objects one action may edit (Spec §7.3 step 9).
const MaxEditedObjects = 10000

// Engine executes action types (Spec §7.3).
type Engine struct {
	Ontologies OntologySource
	Objects    ObjectLoader
	Authz      authz.Authorizer
	Committer  Committer
	Functions  *FunctionRegistry
	// Dispatcher runs write-back and side-effect workflows. nil disables both: write-back actions
	// are rejected with 501 and side effects stay PENDING.
	Dispatcher Dispatcher
	// SyncWait is how long apply waits for a write-back workflow before returning 202 (default 10s).
	SyncWait time.Duration
	// StaleRetries is how many times a STALE_OBJECT conflict is retried from step 4 (default 1).
	StaleRetries int
	// Runtimes holds out-of-process function runtimes by name (typescript, python).
	Runtimes map[string]*RemoteRuntime
}

func newActionRID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("ri.action-run.%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// requestHash fingerprints the request for idempotency-key reuse detection.
// encoding/json sorts map keys, so the hash is stable for equal parameters.
func requestHash(actionType string, params map[string]any) string {
	b, _ := json.Marshal(struct {
		A string         `json:"a"`
		P map[string]any `json:"p"`
	}{actionType, params})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) resolveOntology(ctx context.Context, v *int64) (*spec.CompiledOntology, error) {
	if v != nil {
		o, err := e.Ontologies.Get(ctx, *v)
		if err != nil {
			return nil, apperr.New(http.StatusNotFound, "ONTOLOGY_NOT_FOUND", "ontology version %d not found", *v).Wrap(err)
		}
		return o, nil
	}
	o, err := e.Ontologies.Active(ctx)
	if err != nil {
		return nil, apperr.New(http.StatusServiceUnavailable, "ONTOLOGY_UNAVAILABLE", "no active ontology").Wrap(err)
	}
	return o, nil
}

// allowed checks one relation; backend failures fail closed with 503 (Spec §8.2).
func (e *Engine) allowed(ctx context.Context, actor Actor, relation, object string) (bool, error) {
	ok, err := e.Authz.Check(ctx, "user:"+actor.ID, relation, object)
	if err != nil {
		return false, apperr.New(http.StatusServiceUnavailable, "AUTHZ_UNAVAILABLE", "authorization check failed").Wrap(err)
	}
	if !ok || actor.Kind != ActorAgent {
		return ok, nil
	}
	if actor.OnBehalfOf == "" {
		return false, nil // an agent must act for a user
	}
	ok, err = e.Authz.Check(ctx, "user:"+actor.OnBehalfOf, relation, object)
	if err != nil {
		return false, apperr.New(http.StatusServiceUnavailable, "AUTHZ_UNAVAILABLE", "authorization check failed").Wrap(err)
	}
	return ok, nil
}

// Apply runs one action (validate-only or apply).
func (e *Engine) Apply(ctx context.Context, req ApplyRequest) (*ApplyResult, error) {
	// 1. resolve ontology + action type
	ont, err := e.resolveOntology(ctx, req.OntologyVersion)
	if err != nil {
		return nil, err
	}
	at, ok := ont.ActionTypes[req.ActionType]
	if !ok {
		return nil, apperr.New(http.StatusNotFound, "ACTION_TYPE_NOT_FOUND", "action type %q not found", req.ActionType)
	}
	if req.Actor.ID == "" {
		return nil, apperr.New(http.StatusUnauthorized, "UNAUTHENTICATED", "missing actor")
	}
	hash := requestHash(req.ActionType, req.Parameters)

	// 2. idempotency
	if req.Mode == ModeApply && req.IdempotencyKey != "" {
		if res, err := e.replay(ctx, req, hash); res != nil || err != nil {
			return res, err
		}
	}

	// 3. parameters
	rp, err := resolveParams(ont, at, req.Parameters)
	if err != nil {
		return nil, err
	}
	env, err := expr.NewEnv(at)
	if err != nil {
		return nil, err
	}
	criteria := make([]*expr.Program, len(at.SubmissionCriteria))
	for i, c := range at.SubmissionCriteria {
		if criteria[i], err = env.Compile(c.Expr); err != nil {
			return nil, fmt.Errorf("criterion %d of %s does not compile: %w", i, at.APIName, err)
		}
	}

	retries := e.StaleRetries
	if retries == 0 {
		retries = 1
	}
	for attempt := 0; ; attempt++ {
		res, err := e.attempt(ctx, req, ont, at, env, criteria, rp, hash)
		if errors.Is(err, objstore.ErrStale) && attempt < retries {
			continue // re-read objects (step 4) and try again
		}
		if errors.Is(err, ErrIdempotencyRace) {
			if r, rerr := e.replay(ctx, req, hash); r != nil || rerr != nil {
				return r, rerr
			}
		}
		return res, mapCommitErr(err)
	}
}

// replay returns the stored result for a reused idempotency key, or (nil, nil) when unused.
func (e *Engine) replay(ctx context.Context, req ApplyRequest, hash string) (*ApplyResult, error) {
	prior, err := e.Committer.FindByIdempotency(ctx, req.Actor.ID, req.ActionType, req.IdempotencyKey)
	if err != nil || prior == nil {
		return nil, err
	}
	if prior.RequestHash != hash {
		return nil, apperr.New(http.StatusConflict, "IDEMPOTENCY_KEY_REUSED",
			"idempotency key was already used with different parameters")
	}
	switch prior.Status {
	case StatusCommitted:
		if prior.Result != nil {
			return prior.Result, nil
		}
	case StatusFailed, StatusCompensated:
		return nil, apperr.New(http.StatusConflict, "ACTION_RUN_FAILED",
			"an earlier run with this idempotency key ended as %s; use a new key to retry", prior.Status).
			WithDetails(map[string]any{"actionRid": prior.ActionRID})
	}
	return &ApplyResult{ActionRID: prior.ActionRID, Status: prior.Status}, nil
}

func (e *Engine) attempt(ctx context.Context, req ApplyRequest, ont *spec.CompiledOntology, at *spec.ActionType,
	env *expr.Env, criteria []*expr.Program, rp *resolvedParams, hash string) (*ApplyResult, error) {

	// 4. load referenced objects; missing or invisible ones are reported identically
	refs := rp.allRefs()
	objs, err := e.Objects.GetMany(ctx, ont, refs)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if _, ok := objs[ref]; !ok {
			return nil, apperr.New(http.StatusBadRequest, "OBJECT_NOT_FOUND", "object %s not found", ref)
		}
		if ot := ont.ObjectTypes[ref.Type]; ot != nil && ot.Authz != nil {
			ok, err := e.allowed(ctx, req.Actor, "viewer", ot.Authz.FGAType+":"+ref.PK)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, apperr.New(http.StatusBadRequest, "OBJECT_NOT_FOUND", "object %s not found", ref)
			}
		}
	}

	// 5. action permission
	if at.Permission != nil {
		ref := rp.refs[spec.ParamRef(at.Permission.On)]
		ot := ont.ObjectTypes[ref.Type]
		if ot == nil || ot.Authz == nil {
			return nil, apperr.New(http.StatusInternalServerError, "INTERNAL", "permission target %s has no authz binding", ref.Type)
		}
		ok, err := e.allowed(ctx, req.Actor, at.Permission.Relation, ot.Authz.FGAType+":"+ref.PK)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apperr.New(http.StatusForbidden, "PERMISSION_DENIED", "not allowed to run %s on %s", at.APIName, ref)
		}
	}

	// 6. submission criteria
	vars := celVars(at, rp, objs, req.Actor)
	if err := e.attachLinks(ctx, ont, at, req.Actor, rp, objs, vars); err != nil {
		return nil, err
	}
	val := Validation{Result: StatusValid, Criteria: make([]CriterionResult, 0, len(criteria))}
	for i, prg := range criteria {
		passed, err := prg.EvalBool(vars)
		cr := CriterionResult{Index: i, Passed: passed && err == nil}
		if !cr.Passed {
			cr.Message = at.SubmissionCriteria[i].Message
			val.Result = "INVALID"
		}
		val.Criteria = append(val.Criteria, cr)
	}
	if val.Result != StatusValid {
		return nil, apperr.New(http.StatusUnprocessableEntity, "SUBMISSION_CRITERIA_FAILED", "%s", firstFailure(val)).
			WithDetails(map[string]any{"criteria": val.Criteria})
	}

	// 7. validate-only stops here (nothing is written)
	if req.Mode == ModeValidate {
		return &ApplyResult{Status: StatusValid, OntologyVersion: ont.Version, Validation: val}, nil
	}

	// 8. build edits
	edits, err := e.buildEdits(ctx, ont, at, env, vars, rp, objs, req.Actor)
	if err != nil {
		return nil, err
	}

	// 9. post-edit validation
	before, err := e.checkEdits(ctx, ont, edits, objs)
	if err != nil {
		return nil, err
	}

	// 10. render side effects now, against the objects that were validated
	effects, err := planEffects(ont, at, env, vars)
	if err != nil {
		return nil, err
	}
	writeback := hasWriteback(at)
	if writeback && e.Dispatcher == nil {
		return nil, apperr.New(http.StatusNotImplemented, "WRITEBACK_NOT_SUPPORTED",
			"action %s uses a WRITEBACK webhook but no workflow dispatcher is configured", at.APIName)
	}
	res := &ApplyResult{Status: StatusCommitted, OntologyVersion: ont.Version, Validation: val, Edits: summarize(edits)}
	in := &CommitInput{
		Ontology: ont, ActionRID: newActionRID(), ActionType: at.APIName, Actor: req.Actor,
		IdempotencyKey: req.IdempotencyKey, RequestHash: hash, Parameters: req.Parameters,
		Edits: edits, Before: before, Effects: effects, Result: res,
	}
	if writeback {
		return e.applyWriteback(ctx, in)
	}

	// 11. commit synchronously, then dispatch side effects (at-least-once)
	committed, err := e.Committer.Commit(ctx, in)
	if err != nil {
		return nil, err
	}
	e.dispatchSideEffects(ctx, committed.ActionRID, len(effects))
	return committed, nil
}

// applyWriteback reserves the run, starts its workflow and waits up to SyncWait (Spec §7.3 step 11).
func (e *Engine) applyWriteback(ctx context.Context, in *CommitInput) (*ApplyResult, error) {
	if err := e.Committer.Reserve(ctx, in); err != nil {
		return nil, err
	}
	wait := e.SyncWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	out, err := e.Dispatcher.RunWriteback(ctx, in.ActionRID, wait)
	if err != nil {
		// The workflow never started: mark the run so its idempotency key does not stick to a ghost.
		_ = e.Committer.MarkRun(context.WithoutCancel(ctx), in.ActionRID, StatusFailed, "WORKFLOW_START_FAILED", err.Error())
		return nil, apperr.New(http.StatusServiceUnavailable, "WORKFLOW_UNAVAILABLE", "could not start the write-back workflow").Wrap(err)
	}
	if out == nil {
		return &ApplyResult{ActionRID: in.ActionRID, Status: StatusWritebackPending,
			OntologyVersion: in.Ontology.Version, Validation: in.Result.Validation, Edits: in.Result.Edits}, nil
	}
	return writebackResult(in.ActionRID, out)
}

func writebackResult(actionRID string, out *WritebackOutcome) (*ApplyResult, error) {
	switch out.Status {
	case StatusCommitted:
		if out.Result != nil {
			return out.Result, nil
		}
		return &ApplyResult{ActionRID: actionRID, Status: StatusCommitted}, nil
	case StatusCompensated:
		return nil, apperr.New(http.StatusConflict, out.ErrorCode, "%s; the external write-back was compensated", out.Message).
			WithDetails(map[string]any{"actionRid": actionRID, "status": StatusCompensated})
	}
	code := out.ErrorCode
	if code == "" {
		code = "WRITEBACK_FAILED"
	}
	status := http.StatusFailedDependency
	if code != "WRITEBACK_FAILED" {
		status = http.StatusConflict
	}
	return nil, apperr.New(status, code, "%s", out.Message).WithDetails(map[string]any{"actionRid": actionRID})
}

func (e *Engine) dispatchSideEffects(ctx context.Context, actionRID string, n int) {
	if n == 0 || e.Dispatcher == nil {
		return
	}
	if err := e.Dispatcher.StartSideEffects(context.WithoutCancel(ctx), actionRID); err != nil {
		// The PENDING rows stay in side_effect_run; a sweeper can restart them.
		slog.WarnContext(ctx, "side-effect workflow not started", "actionRid", actionRID, "err", err)
	}
}

func (e *Engine) buildEdits(ctx context.Context, ont *spec.CompiledOntology, at *spec.ActionType, env *expr.Env,
	vars map[string]any, rp *resolvedParams, objs map[objstore.ObjectRef]*objstore.Object, actor Actor) ([]objstore.Edit, error) {

	if at.Function == nil {
		return expandRules(ont, at, env, vars, rp, objs)
	}
	var fn Func
	switch rt := e.Runtimes[at.Function.Runtime]; {
	case at.Function.Runtime == "go" && e.Functions != nil:
		f, ok := e.Functions.Lookup(at.Function.Ref)
		if !ok {
			return nil, apperr.New(http.StatusNotImplemented, "FUNCTION_NOT_REGISTERED", "function %q is not registered", at.Function.Ref)
		}
		fn = f
	case rt != nil:
		fn = rt.Func(ont, at.Function.Ref)
	default:
		return nil, apperr.New(http.StatusNotImplemented, "FUNCTION_RUNTIME_UNAVAILABLE",
			"function runtime %q is not configured", at.Function.Runtime)
	}
	in := FuncInput{
		Params: rp.values, Objects: map[string]*objstore.Object{}, Lists: map[string][]*objstore.Object{},
		Reader: &scopedReader{ont: ont, loader: e.Objects, e: e, actor: actor}, Actor: actor,
	}
	for name, ref := range rp.refs {
		in.Objects[name] = objs[ref]
	}
	for name, list := range rp.refLists {
		for _, ref := range list {
			in.Lists[name] = append(in.Lists[name], objs[ref])
		}
	}
	edits, err := fn(ctx, in)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, apperr.New(http.StatusInternalServerError, "FUNCTION_FAILED", "function %s failed", at.Function.Ref).Wrap(err)
	}
	return edits, nil
}

// checkEdits enforces types, constraints and required properties on the post-edit state, and
// returns the pre-edit property snapshot of each modified/deleted object (for edits.object_edit.before).
func (e *Engine) checkEdits(ctx context.Context, ont *spec.CompiledOntology, edits []objstore.Edit,
	known map[objstore.ObjectRef]*objstore.Object) (map[objstore.ObjectRef]map[string]any, error) {

	fail := func(format string, args ...any) error {
		return apperr.New(http.StatusUnprocessableEntity, "EDIT_VALIDATION_FAILED", format, args...)
	}
	touched := map[objstore.ObjectRef]bool{}
	var missing []objstore.ObjectRef
	for _, ed := range edits {
		touched[ed.Ref] = true
		if ed.Op == objstore.OpLink || ed.Op == objstore.OpUnlink {
			touched[ed.Target] = true
		}
		if ed.Op == objstore.OpModify || ed.Op == objstore.OpDelete {
			if _, ok := known[ed.Ref]; !ok {
				missing = append(missing, ed.Ref)
			}
		}
	}
	if len(touched) > MaxEditedObjects {
		return nil, apperr.New(http.StatusRequestEntityTooLarge, "TOO_MANY_EDITS",
			"action edits %d objects; the limit is %d", len(touched), MaxEditedObjects)
	}
	current := map[objstore.ObjectRef]*objstore.Object{}
	for k, v := range known {
		current[k] = v
	}
	if len(missing) > 0 { // objects first touched by a function
		loaded, err := e.Objects.GetMany(ctx, ont, missing)
		if err != nil {
			return nil, err
		}
		for k, v := range loaded {
			current[k] = v
		}
	}

	state := map[objstore.ObjectRef]map[string]any{}
	before := map[objstore.ObjectRef]map[string]any{}
	for _, ed := range edits {
		if ed.Op == objstore.OpLink || ed.Op == objstore.OpUnlink {
			if _, ok := ont.LinkTypes[ed.LinkType]; !ok {
				return nil, fail("unknown link type %q", ed.LinkType)
			}
			continue
		}
		ot := ont.ObjectTypes[ed.Ref.Type]
		if ot == nil {
			return nil, fail("unknown object type %q", ed.Ref.Type)
		}
		for prop, v := range ed.Changes {
			p := ot.Properties[prop]
			if p == nil {
				return nil, fail("%s: unknown property %q", ed.Ref, prop)
			}
			if err := checkProperty(ont, p, v); err != nil {
				return nil, fail("%s.%s: %v", ed.Ref, prop, err)
			}
		}
		switch ed.Op {
		case objstore.OpCreate:
			m := map[string]any{}
			for k, v := range ed.Changes {
				m[k] = v
			}
			m[ot.PrimaryKey] = ed.Ref.PK
			state[ed.Ref] = m
		case objstore.OpModify, objstore.OpDelete:
			m, ok := state[ed.Ref]
			if !ok {
				o := current[ed.Ref]
				if o == nil {
					return nil, apperr.New(http.StatusBadRequest, "OBJECT_NOT_FOUND", "object %s not found", ed.Ref)
				}
				before[ed.Ref] = objectMap(o)
				m = objectMap(o)
			}
			if ed.Op == objstore.OpDelete {
				state[ed.Ref] = nil
				continue
			}
			for k, v := range ed.Changes {
				m[k] = v
			}
			state[ed.Ref] = m
		}
	}
	for ref, m := range state {
		if m == nil {
			continue
		}
		ot := ont.ObjectTypes[ref.Type]
		for _, name := range ot.PropertyOrder {
			if p := ot.Properties[name]; p.Required && p.Type.Base != spec.TVector && m[name] == nil {
				return nil, fail("%s: required property %q is missing after the edit", ref, name)
			}
		}
	}
	return before, nil
}

func hasWriteback(at *spec.ActionType) bool {
	for _, se := range at.SideEffects {
		if se.Webhook != "" && se.WebhookMode == spec.WebhookWriteback {
			return true
		}
	}
	return false
}

func firstFailure(v Validation) string {
	for _, c := range v.Criteria {
		if !c.Passed {
			if c.Message != "" {
				return c.Message
			}
			return fmt.Sprintf("submission criterion %d failed", c.Index)
		}
	}
	return "submission criteria failed"
}

func summarize(edits []objstore.Edit) EditSummary {
	s := EditSummary{Created: []RefJSON{}, Modified: []RefJSON{}, Deleted: []RefJSON{}, Links: []LinkJSON{}}
	seen := map[string]bool{}
	add := func(list *[]RefJSON, op string, r objstore.ObjectRef) {
		key := op + "|" + r.String()
		if !seen[key] {
			seen[key] = true
			*list = append(*list, RefJSON{Type: r.Type, PK: r.PK})
		}
	}
	for _, ed := range edits {
		switch ed.Op {
		case objstore.OpCreate:
			add(&s.Created, "c", ed.Ref)
		case objstore.OpModify:
			add(&s.Modified, "m", ed.Ref)
		case objstore.OpDelete:
			add(&s.Deleted, "d", ed.Ref)
		default:
			s.Links = append(s.Links, LinkJSON{Op: string(ed.Op), LinkType: ed.LinkType,
				From: RefJSON{ed.Ref.Type, ed.Ref.PK}, To: RefJSON{ed.Target.Type, ed.Target.PK}})
		}
	}
	for _, l := range [][]RefJSON{s.Created, s.Modified, s.Deleted} {
		sort.Slice(l, func(i, j int) bool { return l[i].Type+l[i].PK < l[j].Type+l[j].PK })
	}
	return s
}

// mapCommitErr converts storage errors into API errors (Spec §7.9).
func mapCommitErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae
	}
	var ee *objstore.EditError
	details := map[string]any{}
	if errors.As(err, &ee) {
		details["edit"] = ee.Index
		details["object"] = ee.Edit.Ref.String()
	}
	switch {
	case errors.Is(err, objstore.ErrStale):
		return apperr.New(http.StatusConflict, "STALE_OBJECT", "an object was modified concurrently; retry").Wrap(err).WithDetails(details)
	case errors.Is(err, objstore.ErrAlreadyExists):
		return apperr.New(http.StatusConflict, "OBJECT_ALREADY_EXISTS", "an object with this primary key already exists").Wrap(err).WithDetails(details)
	case errors.Is(err, objstore.ErrNotFound):
		return apperr.New(http.StatusBadRequest, "OBJECT_NOT_FOUND", "an edited object no longer exists").Wrap(err).WithDetails(details)
	case errors.Is(err, objstore.ErrInvalidEdit), errors.Is(err, objstore.ErrInvalidValue), errors.Is(err, objstore.ErrUnknownType):
		return apperr.New(http.StatusUnprocessableEntity, "EDIT_VALIDATION_FAILED", "%v", err).WithDetails(details)
	}
	return err
}
