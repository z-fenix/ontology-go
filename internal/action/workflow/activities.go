package workflow

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/acme/ontology/internal/action"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/webhook"
)

// RunStore is the persistence the activities need (satisfied by *action.PGCommitter).
type RunStore interface {
	LoadPlan(ctx context.Context, actionRID string) (*action.Plan, error)
	LoadEffects(ctx context.Context, actionRID string) ([]action.PlannedEffect, error)
	CommitReserved(ctx context.Context, in *action.CommitInput) (*action.ApplyResult, error)
	RecordEffect(ctx context.Context, actionRID string, a action.EffectAttempt) error
	EffectSucceeded(ctx context.Context, actionRID, name, mode string) (bool, error)
	MarkRun(ctx context.Context, actionRID, status, code, message string) error
}

// WebhookCaller performs one HTTP call (satisfied by *webhook.Executor).
type WebhookCaller interface {
	Do(ctx context.Context, req webhook.Request, idempotencyKey string) (*webhook.Response, error)
}

// Notifier delivers a notification side effect.
type Notifier interface {
	Notify(ctx context.Context, to, template string, data map[string]any) error
}

// Activities are registered on the worker; all are idempotent so Temporal retries are safe.
type Activities struct {
	Store      RunStore
	Ontologies action.OntologySource
	Webhooks   WebhookCaller
	Notifier   Notifier
}

// Summarize reads the stored plan of a reserved run.
func (a *Activities) Summarize(ctx context.Context, rid string) (PlanSummary, error) {
	p, err := a.Store.LoadPlan(ctx, rid)
	if err != nil {
		return PlanSummary{}, err
	}
	var s PlanSummary
	for _, ef := range p.Effects {
		switch ef.Mode {
		case action.ModeWriteback:
			s.Writeback = ef.Name
			s.HasCompensate = ef.Compensate != nil
			if ef.Request != nil {
				s.RetryMax, s.TimeoutMs = ef.Request.Retry.Max, ef.Request.TimeoutMs
			}
		case action.ModeSideEffect:
			s.SideEffectCount++
			s.SideEffectNames = append(s.SideEffectNames, ef.Name)
		}
	}
	return s, nil
}

// CommitRun commits the reserved edits. Business conflicts are non-retryable so the workflow
// can compensate immediately; infrastructure errors are retried.
func (a *Activities) CommitRun(ctx context.Context, rid string) (*action.ApplyResult, error) {
	p, err := a.Store.LoadPlan(ctx, rid)
	if err != nil {
		return nil, err
	}
	ont, err := a.Ontologies.Get(ctx, p.OntologyVersion)
	if err != nil {
		return nil, err
	}
	res, err := a.Store.CommitReserved(ctx, p.CommitInput(ont, rid))
	if err == nil {
		return res, nil
	}
	switch {
	case errors.Is(err, objstore.ErrStale):
		return nil, temporal.NewNonRetryableApplicationError("an edited object changed after validation", "STALE_OBJECT", err)
	case errors.Is(err, objstore.ErrAlreadyExists):
		return nil, temporal.NewNonRetryableApplicationError("a created object already exists", "OBJECT_ALREADY_EXISTS", err)
	case errors.Is(err, objstore.ErrNotFound):
		return nil, temporal.NewNonRetryableApplicationError("an edited object no longer exists", "OBJECT_NOT_FOUND", err)
	case errors.Is(err, objstore.ErrInvalidEdit), errors.Is(err, objstore.ErrInvalidValue), errors.Is(err, action.ErrRunState):
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), ErrCommitFailed, err)
	}
	return nil, err
}

// ListSideEffects returns the names of a committed run's SIDE_EFFECT effects.
func (a *Activities) ListSideEffects(ctx context.Context, rid string) ([]string, error) {
	effects, err := a.Store.LoadEffects(ctx, rid)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(effects))
	for _, ef := range effects {
		names = append(names, ef.Name)
	}
	return names, nil
}

func (a *Activities) loadEffect(ctx context.Context, in EffectInput) (*action.PlannedEffect, *webhook.Request, error) {
	if in.Mode == action.ModeSideEffect {
		effects, err := a.Store.LoadEffects(ctx, in.ActionRID)
		if err != nil {
			return nil, nil, err
		}
		for i := range effects {
			if effects[i].Name == in.Name {
				return &effects[i], effects[i].Request, nil
			}
		}
		return nil, nil, fmt.Errorf("side effect %s not found", in.Name)
	}
	p, err := a.Store.LoadPlan(ctx, in.ActionRID)
	if err != nil {
		return nil, nil, err
	}
	return p.Effect(in.Name, in.Mode)
}

// ExecuteEffect runs one webhook or notification and records every attempt. A success that was
// already recorded is not re-sent; the remote side also receives a stable Idempotency-Key.
func (a *Activities) ExecuteEffect(ctx context.Context, in EffectInput) error {
	done, err := a.Store.EffectSucceeded(ctx, in.ActionRID, in.Name, in.Mode)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	ef, req, err := a.loadEffect(ctx, in)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(err.Error(), "EFFECT_NOT_FOUND", err)
	}
	attempt := int(activity.GetInfo(ctx).Attempt)
	rec := action.EffectAttempt{Kind: ef.Kind, Name: in.Name, Mode: in.Mode, Attempt: attempt, Request: req}

	var callErr error
	switch ef.Kind {
	case action.EffectWebhook:
		if req == nil {
			callErr = temporal.NewNonRetryableApplicationError("webhook effect has no request", "EFFECT_INVALID", nil)
			break
		}
		var resp *webhook.Response
		resp, callErr = a.Webhooks.Do(ctx, *req, in.ActionRID+":"+in.Name+":"+in.Mode)
		rec.Response = resp
	case action.EffectNotify:
		if a.Notifier == nil {
			callErr = temporal.NewNonRetryableApplicationError("no notifier configured", "NOTIFIER_UNAVAILABLE", nil)
			break
		}
		callErr = a.Notifier.Notify(ctx, ef.NotifyTo, ef.Template, map[string]any{"actionRid": in.ActionRID})
	default:
		callErr = temporal.NewNonRetryableApplicationError("unknown effect kind "+ef.Kind, "EFFECT_INVALID", nil)
	}

	rec.Status = "SUCCEEDED"
	if callErr != nil {
		rec.Status, rec.Error = "FAILED", callErr.Error()
		if webhook.IsPermanent(callErr) {
			callErr = temporal.NewNonRetryableApplicationError(callErr.Error(), "WEBHOOK_REJECTED", callErr)
		}
	}
	if err := a.Store.RecordEffect(ctx, in.ActionRID, rec); err != nil && callErr == nil {
		return err // the call succeeded but was not recorded; retry so the record lands (remote deduplicates)
	}
	return callErr
}

// MarkRun sets a run's final status.
func (a *Activities) MarkRun(ctx context.Context, rid, status, code, msg string) error {
	return a.Store.MarkRun(ctx, rid, status, code, msg)
}
