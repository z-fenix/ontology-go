package action

import (
	"context"
	"fmt"
	"time"
)

var _ SweepStore = (*PGCommitter)(nil)

// StuckRuns returns write-back runs and committed runs with pending side effects that have not been
// updated for olderThan, oldest first (at most limit of each kind).
func (c *PGCommitter) StuckRuns(ctx context.Context, olderThan time.Duration, limit int) ([]StuckRun, error) {
	rows, err := c.Pool.Query(ctx, `
(SELECT action_rid, 'writeback' FROM edits.action_run
  WHERE status IN ('WRITEBACK_PENDING', 'COMPENSATING') AND updated_at < now() - make_interval(secs => $1)
  ORDER BY updated_at LIMIT $2)
UNION ALL
(SELECT s.action_rid, 'side_effects' FROM edits.side_effect_run s
  JOIN edits.action_run r ON r.action_rid = s.action_rid
  WHERE r.status = 'COMMITTED' AND s.mode = 'SIDE_EFFECT' AND s.status = 'PENDING'
    AND s.updated_at < now() - make_interval(secs => $1)
  GROUP BY s.action_rid ORDER BY min(s.updated_at) LIMIT $2)`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckRun
	for rows.Next() {
		var r StuckRun
		if err := rows.Scan(&r.ActionRID, &r.Kind); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchRun bumps updated_at of a stuck run (or of its pending side-effect rows).
func (c *PGCommitter) TouchRun(ctx context.Context, r StuckRun) error {
	var q string
	switch r.Kind {
	case SweepWriteback:
		q = `UPDATE edits.action_run SET updated_at = now()
WHERE action_rid = $1 AND status IN ('WRITEBACK_PENDING', 'COMPENSATING')`
	case SweepSideEffects:
		q = `UPDATE edits.side_effect_run SET updated_at = now()
WHERE action_rid = $1 AND mode = 'SIDE_EFFECT' AND status = 'PENDING'`
	default:
		return fmt.Errorf("unknown sweep kind %q", r.Kind)
	}
	_, err := c.Pool.Exec(ctx, q, r.ActionRID)
	return err
}
