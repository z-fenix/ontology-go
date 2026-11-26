package action

import (
	"context"
	"log/slog"
	"time"
)

// Kinds of stuck work found by the sweeper.
const (
	SweepWriteback   = "writeback"
	SweepSideEffects = "side_effects"
)

// StuckRun is a run whose workflow may have been lost: a write-back run still WRITEBACK_PENDING or
// COMPENSATING, or a committed run whose SIDE_EFFECT rows are still PENDING, untouched for a while.
type StuckRun struct {
	ActionRID string
	Kind      string
}

// SweepStore finds and marks stuck runs (implemented by PGCommitter).
type SweepStore interface {
	StuckRuns(ctx context.Context, olderThan time.Duration, limit int) ([]StuckRun, error)
	// TouchRun bumps updated_at so the run is not picked again until olderThan has passed.
	TouchRun(ctx context.Context, r StuckRun) error
	MarkRun(ctx context.Context, actionRID, status, code, message string) error
}

// Sweeper re-dispatches work whose workflow was never started or whose outcome was never recorded,
// e.g. when the process stopped between reserving a run and starting its workflow, or when the
// side-effect workflow could not be started after a commit. Re-dispatching is safe: workflow IDs
// derive from the action RID, so a running or finished workflow is attached to, never duplicated.
// Several replicas may sweep at the same time for the same reason.
type Sweeper struct {
	Store      SweepStore
	Dispatcher Dispatcher
	// OlderThan is how long a run must be untouched before it is swept (default 5m).
	OlderThan time.Duration
	// Interval is the time between sweeps (default 1m).
	Interval time.Duration
	// Batch is the most runs handled per sweep (default 100).
	Batch int
	// Wait is how long to wait on a re-attached write-back workflow (default 2s).
	Wait time.Duration
}

// SweepStats reports one sweep.
type SweepStats struct {
	Writebacks  int
	Resolved    int
	SideEffects int
	Errors      int
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// SweepOnce handles up to Batch stuck runs.
func (s *Sweeper) SweepOnce(ctx context.Context) (SweepStats, error) {
	var st SweepStats
	batch := s.Batch
	if batch <= 0 {
		batch = 100
	}
	runs, err := s.Store.StuckRuns(ctx, durationOr(s.OlderThan, 5*time.Minute), batch)
	if err != nil {
		return st, err
	}
	for _, r := range runs {
		switch r.Kind {
		case SweepWriteback:
			st.Writebacks++
			out, err := s.Dispatcher.RunWriteback(ctx, r.ActionRID, durationOr(s.Wait, 2*time.Second))
			if err != nil {
				st.Errors++
				slog.WarnContext(ctx, "sweeper: cannot re-dispatch write-back", "actionRid", r.ActionRID, "err", err)
				continue
			}
			if out == nil {
				// Still running: look again after OlderThan.
				if err := s.Store.TouchRun(ctx, r); err != nil {
					st.Errors++
				}
				continue
			}
			st.Resolved++
			if out.Status != StatusCommitted {
				// The workflow finished but recording its outcome failed; record it now.
				if err := s.Store.MarkRun(ctx, r.ActionRID, out.Status, out.ErrorCode, out.Message); err != nil {
					st.Errors++
				}
			}
		case SweepSideEffects:
			st.SideEffects++
			if err := s.Dispatcher.StartSideEffects(ctx, r.ActionRID); err != nil {
				st.Errors++
				slog.WarnContext(ctx, "sweeper: cannot restart side effects", "actionRid", r.ActionRID, "err", err)
				continue
			}
			if err := s.Store.TouchRun(ctx, r); err != nil {
				st.Errors++
			}
		}
	}
	return st, nil
}

// Run sweeps every Interval until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(durationOr(s.Interval, time.Minute))
	defer t.Stop()
	for {
		st, err := s.SweepOnce(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "sweeper failed", "err", err)
			}
		case st != (SweepStats{}):
			slog.InfoContext(ctx, "sweeper", "writebacks", st.Writebacks, "resolved", st.Resolved,
				"sideEffects", st.SideEffects, "errors", st.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
