package action

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/acme/ontology/internal/expr"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
	"github.com/acme/ontology/internal/webhook"
)

// Effect kinds and modes recorded in edits.side_effect_run.
const (
	EffectWebhook = "webhook"
	EffectNotify  = "notify"

	ModeWriteback  = "WRITEBACK"
	ModeSideEffect = "SIDE_EFFECT"
	ModeCompensate = "COMPENSATE"
)

// Run statuses beyond VALIDATED/COMMITTED (Spec §7.2).
const (
	StatusWritebackPending = "WRITEBACK_PENDING"
	StatusFailed           = "FAILED"
	StatusCompensating     = "COMPENSATING"
	StatusCompensated      = "COMPENSATED"
)

// PlannedEffect is a side effect with all templates already rendered, so the workflow can run
// it later without re-evaluating CEL against objects that may have changed meanwhile.
type PlannedEffect struct {
	Kind       string           `json:"kind"`
	Name       string           `json:"name"` // unique within the run
	Mode       string           `json:"mode"`
	Request    *webhook.Request `json:"request,omitempty"`
	Compensate *webhook.Request `json:"compensate,omitempty"`
	NotifyTo   string           `json:"notifyTo,omitempty"`
	Template   string           `json:"template,omitempty"`
}

// BeforeSnapshot is the pre-edit state of one object (JSON-friendly form of CommitInput.Before).
type BeforeSnapshot struct {
	Ref   objstore.ObjectRef `json:"ref"`
	Props map[string]any     `json:"props"`
}

// Plan is everything needed to commit a run later; stored in edits.action_run.plan.
type Plan struct {
	ActionType      string           `json:"actionType"`
	OntologyVersion int64            `json:"ontologyVersion"`
	Actor           Actor            `json:"actor"`
	IdempotencyKey  string           `json:"idempotencyKey,omitempty"`
	RequestHash     string           `json:"requestHash"`
	Parameters      map[string]any   `json:"parameters"`
	Edits           []objstore.Edit  `json:"edits"`
	Before          []BeforeSnapshot `json:"before,omitempty"`
	Effects         []PlannedEffect  `json:"effects,omitempty"`
	Result          ApplyResult      `json:"result"`
}

// NewPlan captures a commit input.
func NewPlan(in *CommitInput) *Plan {
	p := &Plan{ActionType: in.ActionType, OntologyVersion: in.Ontology.Version, Actor: in.Actor,
		IdempotencyKey: in.IdempotencyKey, RequestHash: in.RequestHash, Parameters: in.Parameters,
		Edits: in.Edits, Effects: in.Effects, Result: *in.Result}
	for ref, props := range in.Before {
		p.Before = append(p.Before, BeforeSnapshot{Ref: ref, Props: props})
	}
	return p
}

// CommitInput rebuilds the commit input for a reserved run.
func (p *Plan) CommitInput(ont *spec.CompiledOntology, actionRID string) *CommitInput {
	before := map[objstore.ObjectRef]map[string]any{}
	for _, b := range p.Before {
		before[b.Ref] = b.Props
	}
	res := p.Result
	return &CommitInput{Ontology: ont, ActionRID: actionRID, ActionType: p.ActionType, Actor: p.Actor,
		IdempotencyKey: p.IdempotencyKey, RequestHash: p.RequestHash, Parameters: p.Parameters,
		Edits: p.Edits, Before: before, Effects: p.Effects, Result: &res}
}

// Effect returns the effect with name and mode. COMPENSATE resolves to the write-back's inverse call.
func (p *Plan) Effect(name, mode string) (*PlannedEffect, *webhook.Request, error) {
	for i := range p.Effects {
		ef := &p.Effects[i]
		if ef.Name != name {
			continue
		}
		switch {
		case mode == ModeCompensate && ef.Mode == ModeWriteback:
			if ef.Compensate == nil {
				return nil, nil, fmt.Errorf("effect %s has no compensate call", name)
			}
			return ef, ef.Compensate, nil
		case ef.Mode == mode:
			return ef, ef.Request, nil
		}
	}
	return nil, nil, fmt.Errorf("effect %s/%s not found in plan", name, mode)
}

// EffectAttempt is one recorded execution of a side effect.
type EffectAttempt struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Mode     string            `json:"mode"`
	Attempt  int               `json:"attempt"`
	Status   string            `json:"status"` // PENDING | SUCCEEDED | FAILED
	Request  *webhook.Request  `json:"request,omitempty"`
	Response *webhook.Response `json:"response,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// WritebackOutcome is the business result of a write-back workflow.
type WritebackOutcome struct {
	Status    string       `json:"status"` // COMMITTED | FAILED | COMPENSATED
	Result    *ApplyResult `json:"result,omitempty"`
	ErrorCode string       `json:"errorCode,omitempty"`
	Message   string       `json:"message,omitempty"`
}

// Dispatcher starts workflows (Temporal implementation in internal/action/workflow).
type Dispatcher interface {
	// RunWriteback starts (or attaches to) the write-back workflow for a reserved run and waits up
	// to wait for its outcome. A nil outcome with nil error means it is still running.
	RunWriteback(ctx context.Context, actionRID string, wait time.Duration) (*WritebackOutcome, error)
	// StartSideEffects starts the side-effect workflow for a committed run without waiting.
	StartSideEffects(ctx context.Context, actionRID string) error
}

// ErrRunState means a reserved run is not in a state that can be committed.
var ErrRunState = errors.New("action run is not awaiting commit")

// ---------- template rendering ----------

func renderString(env *expr.Env, vars map[string]any, s string) (string, error) {
	t := expr.ParseTemplate(s)
	if len(t.Exprs()) == 0 {
		return s, nil
	}
	v, err := t.Render(env, vars)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(v), nil
}

func renderAny(env *expr.Env, vars map[string]any, v any) (any, error) {
	switch x := v.(type) {
	case string:
		t := expr.ParseTemplate(x)
		if len(t.Exprs()) == 0 {
			return x, nil
		}
		return t.Render(env, vars) // a whole-expression template keeps its type
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			r, err := renderAny(env, vars, item)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			r, err := renderAny(env, vars, item)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func renderRequest(env *expr.Env, vars map[string]any, wh *spec.Webhook, method, url string, body map[string]any) (*webhook.Request, error) {
	u, err := renderString(env, vars, url)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	var b map[string]any
	if body != nil {
		r, err := renderAny(env, vars, body)
		if err != nil {
			return nil, fmt.Errorf("body.%w", err)
		}
		b = r.(map[string]any)
	}
	return &webhook.Request{Name: wh.Name, Method: method, URL: u, Body: b,
		TimeoutMs: wh.TimeoutMs, Retry: wh.Retry, Auth: wh.Auth}, nil
}

// planEffects renders every side effect of an action against the request's variables.
func planEffects(ont *spec.CompiledOntology, at *spec.ActionType, env *expr.Env, vars map[string]any) ([]PlannedEffect, error) {
	fail := func(i int, err error) error {
		return apperr.New(http.StatusUnprocessableEntity, "SIDE_EFFECT_RENDER_FAILED", "side effect %d: %v", i, err)
	}
	used := map[string]bool{}
	uniq := func(name string, i int) string {
		if used[name] {
			name = fmt.Sprintf("%s#%d", name, i)
		}
		used[name] = true
		return name
	}
	var out []PlannedEffect
	for i, se := range at.SideEffects {
		if se.Webhook != "" {
			wh := ont.Webhooks[se.Webhook]
			if wh == nil {
				return nil, fail(i, fmt.Errorf("webhook %q is not defined", se.Webhook))
			}
			req, err := renderRequest(env, vars, wh, wh.Method, wh.URL, wh.Body)
			if err != nil {
				return nil, fail(i, err)
			}
			mode := string(se.WebhookMode)
			if mode == "" {
				mode = ModeSideEffect
			}
			pe := PlannedEffect{Kind: EffectWebhook, Name: uniq(se.Webhook, i), Mode: mode, Request: req}
			if mode == ModeWriteback && wh.Compensate != nil {
				if pe.Compensate, err = renderRequest(env, vars, wh, wh.Compensate.Method, wh.Compensate.URL, wh.Compensate.Body); err != nil {
					return nil, fail(i, fmt.Errorf("compensate: %w", err))
				}
			}
			out = append(out, pe)
			continue
		}
		to, err := renderString(env, vars, se.NotifyTo)
		if err != nil {
			return nil, fail(i, err)
		}
		out = append(out, PlannedEffect{Kind: EffectNotify, Name: uniq("notify:"+se.Template, i), Mode: ModeSideEffect,
			NotifyTo: to, Template: se.Template})
	}
	return out, nil
}
