package workflow

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/acme/ontology/internal/action"
)

// Dispatcher starts workflows through a Temporal client (implements action.Dispatcher).
type Dispatcher struct {
	Client client.Client
}

func startOptions(id string) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:        id,
		TaskQueue: TaskQueue,
		// One workflow per run, ever: a duplicate start attaches to the existing execution.
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}
}

// RunWriteback starts ActionWorkflow and waits up to wait for its outcome.
func (d *Dispatcher) RunWriteback(ctx context.Context, actionRID string, wait time.Duration) (*action.WritebackOutcome, error) {
	run, err := d.Client.ExecuteWorkflow(ctx, startOptions(WorkflowID(actionRID)), ActionWorkflow, Input{ActionRID: actionRID})
	if err != nil {
		var started *serviceerror.WorkflowExecutionAlreadyStarted
		if !errors.As(err, &started) {
			return nil, err
		}
		run = d.Client.GetWorkflow(ctx, WorkflowID(actionRID), "")
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var out action.WritebackOutcome
	if err := run.Get(wctx, &out); err != nil {
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			return nil, nil // still running: the caller returns 202 WRITEBACK_PENDING
		}
		return nil, err
	}
	return &out, nil
}

// StartSideEffects starts SideEffectsWorkflow without waiting.
func (d *Dispatcher) StartSideEffects(ctx context.Context, actionRID string) error {
	_, err := d.Client.ExecuteWorkflow(ctx, startOptions(SideEffectsWorkflowID(actionRID)), SideEffectsWorkflow, Input{ActionRID: actionRID})
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	return err
}
