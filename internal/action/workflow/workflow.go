// Package workflow orchestrates write-back actions and side effects with Temporal (Spec §7.5).
//
// ActionWorkflow (one per write-back run, ID "action-run:<rid>"):
//  1. ExecuteEffect(WRITEBACK)  — retried per the webhook's retry policy; on failure the run is
//     FAILED and the ontology is untouched.
//  2. CommitRun                 — idempotent; retried on infrastructure errors only.
//  3. If the commit fails after a successful write-back, ExecuteEffect(COMPENSATE) runs the
//     webhook's inverse call and the run ends COMPENSATED (or FAILED with an inconsistency
//     alert when no compensate call exists or it fails).
//  4. Remaining SIDE_EFFECT webhooks / notifications run in SideEffectsWorkflow.
//
// SideEffectsWorkflow ("side-effects:<rid>") runs each effect independently; a failing effect
// never rolls back committed edits.
package workflow

import (
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/acme/ontology/internal/action"
)

// TaskQueue is the Temporal task queue served by the action-service worker.
const TaskQueue = "action-service"

// Application error types. Activities return them as non-retryable so the workflow can branch.
const (
	ErrWritebackFailed = "WRITEBACK_FAILED"
	ErrCommitFailed    = "COMMIT_FAILED"
)

// Input identifies the run; everything else is loaded from the stored plan.
type Input struct {
	ActionRID string `json:"actionRid"`
}

// EffectInput identifies one effect of a run.
type EffectInput struct {
	ActionRID string `json:"actionRid"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
}

// PlanSummary is what the workflow needs from the plan (kept small: it lands in history).
type PlanSummary struct {
	Writeback        string   `json:"writeback"`
	HasCompensate    bool     `json:"hasCompensate"`
	RetryMax         int      `json:"retryMax"`
	TimeoutMs        int      `json:"timeoutMs"`
	SideEffectCount  int      `json:"sideEffectCount"`
	SideEffectNames  []string `json:"sideEffectNames"`
}

// WorkflowID returns the workflow ID for a write-back run.
func WorkflowID(actionRID string) string { return "action-run:" + actionRID }

// SideEffectsWorkflowID returns the workflow ID for a run's side effects.
func SideEffectsWorkflowID(actionRID string) string { return "side-effects:" + actionRID }

func webhookOptions(maxAttempts, timeoutMs int) workflow.ActivityOptions {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return workflow.ActivityOptions{
		StartToCloseTimeout: timeout + 5*time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    int32(maxAttempts),
		},
	}
}

var dbOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 30 * time.Second,
	RetryPolicy: &temporal.RetryPolicy{
		InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Second, MaximumAttempts: 5,
	},
}

func fail(ctx workflow.Context, rid, status, code, msg string) (action.WritebackOutcome, error) {
	ctx = workflow.WithActivityOptions(ctx, dbOptions)
	var a *Activities
	_ = workflow.ExecuteActivity(ctx, a.MarkRun, rid, status, code, msg).Get(ctx, nil)
	return action.WritebackOutcome{Status: status, ErrorCode: code, Message: msg}, nil
}

func errorCode(err error, fallback string) string {
	var ae *temporal.ApplicationError
	if temporal.IsApplicationError(err) && asApp(err, &ae) && ae.Type() != "" && ae.Type() != ErrCommitFailed {
		return ae.Type()
	}
	return fallback
}

// ActionWorkflow runs write-back → commit → compensate-on-failure → side effects.
func ActionWorkflow(ctx workflow.Context, in Input) (action.WritebackOutcome, error) {
	var a *Activities
	rid := in.ActionRID

	var sum PlanSummary
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, dbOptions), a.Summarize, rid).Get(ctx, &sum); err != nil {
		return fail(ctx, rid, action.StatusFailed, "PLAN_UNAVAILABLE", err.Error())
	}

	// 1. write-back (external system first; failure leaves the ontology untouched)
	if sum.Writeback != "" {
		wctx := workflow.WithActivityOptions(ctx, webhookOptions(sum.RetryMax, sum.TimeoutMs))
		err := workflow.ExecuteActivity(wctx, a.ExecuteEffect, EffectInput{rid, sum.Writeback, action.ModeWriteback}).Get(ctx, nil)
		if err != nil {
			return fail(ctx, rid, action.StatusFailed, ErrWritebackFailed, fmt.Sprintf("write-back %s failed: %v", sum.Writeback, err))
		}
	}

	// 2. commit the reserved edits
	var res action.ApplyResult
	cerr := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, dbOptions), a.CommitRun, rid).Get(ctx, &res)
	if cerr == nil {
		if sum.SideEffectCount > 0 {
			startSideEffects(ctx, rid)
		}
		return action.WritebackOutcome{Status: action.StatusCommitted, Result: &res}, nil
	}
	code := errorCode(cerr, ErrCommitFailed)

	// 3. the external system changed but the ontology did not: compensate
	if sum.Writeback == "" {
		return fail(ctx, rid, action.StatusFailed, code, cerr.Error())
	}
	if !sum.HasCompensate {
		workflow.GetLogger(ctx).Error("WRITEBACK_INCONSISTENT: commit failed after write-back and no compensate call is defined",
			"actionRid", rid, "webhook", sum.Writeback, "err", cerr)
		return fail(ctx, rid, action.StatusFailed, "WRITEBACK_INCONSISTENT", cerr.Error())
	}
	_ = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, dbOptions), a.MarkRun, rid,
		action.StatusCompensating, code, cerr.Error()).Get(ctx, nil)
	compCtx := workflow.WithActivityOptions(ctx, webhookOptions(sum.RetryMax*2, sum.TimeoutMs))
	if err := workflow.ExecuteActivity(compCtx, a.ExecuteEffect, EffectInput{rid, sum.Writeback, action.ModeCompensate}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("WRITEBACK_INCONSISTENT: compensation failed", "actionRid", rid, "err", err)
		return fail(ctx, rid, action.StatusFailed, "WRITEBACK_INCONSISTENT", fmt.Sprintf("commit failed (%v) and compensation failed (%v)", cerr, err))
	}
	return fail(ctx, rid, action.StatusCompensated, code, cerr.Error())
}

// startSideEffects starts the side-effect workflow as an abandoned child so it outlives this one.
func startSideEffects(ctx workflow.Context, rid string) {
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID: SideEffectsWorkflowID(rid), TaskQueue: TaskQueue,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	f := workflow.ExecuteChildWorkflow(cctx, SideEffectsWorkflow, Input{ActionRID: rid})
	_ = f.GetChildWorkflowExecution().Get(ctx, nil) // wait until started, not until finished
}

// SideEffectsWorkflow runs every SIDE_EFFECT of a committed run; failures are recorded only.
func SideEffectsWorkflow(ctx workflow.Context, in Input) error {
	var a *Activities
	var names []string
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, dbOptions), a.ListSideEffects, in.ActionRID).Get(ctx, &names); err != nil {
		return err
	}
	futures := make([]workflow.Future, 0, len(names))
	for _, name := range names {
		ectx := workflow.WithActivityOptions(ctx, webhookOptions(5, 10000))
		futures = append(futures, workflow.ExecuteActivity(ectx, a.ExecuteEffect, EffectInput{in.ActionRID, name, action.ModeSideEffect}))
	}
	for i, f := range futures {
		if err := f.Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Warn("side effect failed", "actionRid", in.ActionRID, "effect", names[i], "err", err)
		}
	}
	return nil
}
