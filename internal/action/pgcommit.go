package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/objstore"
)

// PGCommitter writes action_run, object edits, outbox events and side-effect rows (Spec §7.4).
// Isolation is READ COMMITTED; correctness relies on optimistic locking.
type PGCommitter struct {
	Pool  *pgxpool.Pool
	Store *objstore.Store
}

// NewPGCommitter builds a committer.
func NewPGCommitter(pool *pgxpool.Pool, store *objstore.Store) *PGCommitter {
	return &PGCommitter{Pool: pool, Store: store}
}

const idemConstraint = "action_run_idem"

func isIdemViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == idemConstraint
}

// FindByIdempotency looks up an earlier run by (actor, action type, key).
func (c *PGCommitter) FindByIdempotency(ctx context.Context, actorID, actionType, key string) (*PriorRun, error) {
	var pr PriorRun
	var result []byte
	err := c.Pool.QueryRow(ctx, `
SELECT action_rid, request_hash, status, result FROM edits.action_run
WHERE actor_id = $1 AND action_type = $2 AND idempotency_key = $3`, actorID, actionType, key).
		Scan(&pr.ActionRID, &pr.RequestHash, &pr.Status, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		pr.Result = &ApplyResult{}
		if err := json.Unmarshal(result, pr.Result); err != nil {
			return nil, fmt.Errorf("decode stored result: %w", err)
		}
	}
	return &pr, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func insertRun(ctx context.Context, tx pgx.Tx, in *CommitInput, status string, plan []byte) error {
	params, err := json.Marshal(in.Parameters)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO edits.action_run (action_rid, ontology, ontology_ver, action_type, actor_id, actor_kind,
  on_behalf_of, idempotency_key, request_hash, parameters, status, plan, workflow_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		in.ActionRID, in.Ontology.APIName, in.Ontology.Version, in.ActionType, in.Actor.ID, string(in.Actor.Kind),
		nullable(in.Actor.OnBehalfOf), nullable(in.IdempotencyKey), in.RequestHash, params, status, plan,
		nullable(workflowIDFor(status, in.ActionRID)))
	if isIdemViolation(err) {
		return ErrIdempotencyRace
	}
	return err
}

func workflowIDFor(status, actionRID string) string {
	if status == StatusWritebackPending {
		return "action-run:" + actionRID
	}
	return ""
}

// Commit applies the edits and records a new run in one transaction.
func (c *PGCommitter) Commit(ctx context.Context, in *CommitInput) (*ApplyResult, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertRun(ctx, tx, in, "VALIDATED", nil); err != nil {
		return nil, err
	}
	res, err := c.applyAndFinish(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit(ctx)
}

// Reserve records a write-back run as WRITEBACK_PENDING with its plan. Nothing is edited yet.
func (c *PGCommitter) Reserve(ctx context.Context, in *CommitInput) error {
	plan, err := json.Marshal(NewPlan(in))
	if err != nil {
		return err
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertRun(ctx, tx, in, StatusWritebackPending, plan); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CommitReserved commits a WRITEBACK_PENDING run. It is idempotent: if the run is already
// COMMITTED (e.g. an activity retry after a lost reply) the stored result is returned.
func (c *PGCommitter) CommitReserved(ctx context.Context, in *CommitInput) (*ApplyResult, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT status, result FROM edits.action_run WHERE action_rid = $1 FOR UPDATE`,
		in.ActionRID).Scan(&status, &stored); err != nil {
		return nil, err
	}
	switch status {
	case StatusCommitted:
		var res ApplyResult
		if err := json.Unmarshal(stored, &res); err != nil {
			return nil, err
		}
		return &res, nil
	case StatusWritebackPending:
	default:
		return nil, fmt.Errorf("%w: run %s is %s", ErrRunState, in.ActionRID, status)
	}
	res, err := c.applyAndFinish(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit(ctx)
}

// applyAndFinish applies edits, writes object_edit + outbox + side-effect rows, and marks the run
// COMMITTED, all inside tx. The write-back effect itself is not re-recorded (it already ran).
func (c *PGCommitter) applyAndFinish(ctx context.Context, tx pgx.Tx, in *CommitInput) (*ApplyResult, error) {
	if _, err := c.Store.ApplyEdits(ctx, tx, in.Ontology, in.Edits); err != nil {
		return nil, err
	}
	var maxEdit int64
	for _, ed := range in.Edits {
		var changes, before any
		var linkType string
		switch ed.Op {
		case objstore.OpLink, objstore.OpUnlink:
			linkType = ed.LinkType
			changes = map[string]any{"target": map[string]string{"type": ed.Target.Type, "pk": ed.Target.PK}}
		default:
			ch := ed.Changes
			if ch == nil {
				ch = map[string]any{}
			}
			changes = ch
			if b, ok := in.Before[ed.Ref]; ok {
				before = b
			}
		}
		cj, err := marshal(changes)
		if err != nil {
			return nil, err
		}
		bj, err := marshal(before)
		if err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRow(ctx, `
INSERT INTO edits.object_edit (action_rid, object_type, object_pk, op, changes, before, link_type)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING edit_id`,
			in.ActionRID, ed.Ref.Type, ed.Ref.PK, string(ed.Op), cj, bj, nullable(linkType)).Scan(&id); err != nil {
			return nil, err
		}
		if id > maxEdit {
			maxEdit = id
		}
		payload, _ := json.Marshal(map[string]any{"actionRid": in.ActionRID, "op": ed.Op, "editId": id,
			"ontologyVersion": in.Ontology.Version})
		if _, err := tx.Exec(ctx, `INSERT INTO edits.outbox (aggregate, aggregate_id, event_type, payload)
VALUES ($1,$2,'ObjectEdited',$3)`, ed.Ref.Type, ed.Ref.PK, payload); err != nil {
			return nil, err
		}
	}

	res := *in.Result
	res.ActionRID = in.ActionRID
	res.Status = StatusCommitted
	if maxEdit > 0 {
		res.ConsistencyToken = fmt.Sprintf("edit:%d", maxEdit)
	}
	resJSON, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO edits.outbox (aggregate, aggregate_id, event_type, payload)
VALUES ('action_run',$1,'ActionCommitted',$2)`, in.ActionRID, resJSON); err != nil {
		return nil, err
	}
	for _, ef := range in.Effects {
		if ef.Mode == ModeWriteback {
			continue
		}
		req, err := marshal(effectRequest(ef))
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO edits.side_effect_run (action_rid, kind, name, mode, status, request)
VALUES ($1,$2,$3,$4,'PENDING',$5) ON CONFLICT (action_rid, name, mode) DO NOTHING`,
			in.ActionRID, ef.Kind, ef.Name, ef.Mode, req); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE edits.action_run SET status='COMMITTED', committed_at=now(), updated_at=now(), result=$2
WHERE action_rid=$1`, in.ActionRID, resJSON); err != nil {
		return nil, err
	}
	return &res, nil
}

// effectRequest is the audit form of an effect (no secrets: auth holds only the secret reference).
func effectRequest(ef PlannedEffect) any {
	if ef.Request != nil {
		return ef.Request
	}
	return map[string]string{"notifyTo": ef.NotifyTo, "template": ef.Template}
}

// LoadPlan returns a reserved or committed run's plan.
func (c *PGCommitter) LoadPlan(ctx context.Context, actionRID string) (*Plan, error) {
	var raw []byte
	if err := c.Pool.QueryRow(ctx, `SELECT plan FROM edits.action_run WHERE action_rid = $1`, actionRID).Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("run %s has no plan", actionRID)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadEffects returns the planned effects of a committed run (from side_effect_run requests).
func (c *PGCommitter) LoadEffects(ctx context.Context, actionRID string) ([]PlannedEffect, error) {
	rows, err := c.Pool.Query(ctx, `SELECT kind, name, mode, request FROM edits.side_effect_run
WHERE action_rid = $1 AND mode = 'SIDE_EFFECT' ORDER BY id`, actionRID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlannedEffect
	for rows.Next() {
		var ef PlannedEffect
		var req []byte
		if err := rows.Scan(&ef.Kind, &ef.Name, &ef.Mode, &req); err != nil {
			return nil, err
		}
		if ef.Kind == EffectWebhook && len(req) > 0 {
			if err := json.Unmarshal(req, &ef.Request); err != nil {
				return nil, err
			}
		} else if len(req) > 0 {
			var n struct{ NotifyTo, Template string }
			_ = json.Unmarshal(req, &n)
			ef.NotifyTo, ef.Template = n.NotifyTo, n.Template
		}
		out = append(out, ef)
	}
	return out, rows.Err()
}

// RecordEffect upserts one side_effect_run row (unique on action_rid, name, mode).
func (c *PGCommitter) RecordEffect(ctx context.Context, actionRID string, a EffectAttempt) error {
	req, err := marshal(a.Request)
	if err != nil {
		return err
	}
	resp, err := marshal(a.Response)
	if err != nil {
		return err
	}
	var errJSON []byte
	if a.Error != "" {
		errJSON, _ = json.Marshal(map[string]string{"message": a.Error})
	}
	_, err = c.Pool.Exec(ctx, `
INSERT INTO edits.side_effect_run (action_rid, kind, name, mode, attempt, status, request, response, error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (action_rid, name, mode) DO UPDATE SET attempt = EXCLUDED.attempt, status = EXCLUDED.status,
  request = COALESCE(EXCLUDED.request, edits.side_effect_run.request), response = EXCLUDED.response,
  error = EXCLUDED.error, updated_at = now()`,
		actionRID, a.Kind, a.Name, a.Mode, a.Attempt, a.Status, req, resp, errJSON)
	return err
}

// EffectSucceeded reports whether an effect already succeeded (activity retries skip re-sending).
func (c *PGCommitter) EffectSucceeded(ctx context.Context, actionRID, name, mode string) (bool, error) {
	var status string
	err := c.Pool.QueryRow(ctx, `SELECT status FROM edits.side_effect_run WHERE action_rid=$1 AND name=$2 AND mode=$3`,
		actionRID, name, mode).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return status == "SUCCEEDED", err
}

// MarkRun sets a run's status and error. It never downgrades a COMMITTED run.
func (c *PGCommitter) MarkRun(ctx context.Context, actionRID, status, code, message string) error {
	var errJSON []byte
	if code != "" || message != "" {
		errJSON, _ = json.Marshal(map[string]string{"errorCode": code, "message": message})
	}
	_, err := c.Pool.Exec(ctx, `UPDATE edits.action_run SET status=$2, error=COALESCE($3, error), updated_at=now()
WHERE action_rid=$1 AND status <> 'COMMITTED'`, actionRID, status, errJSON)
	return err
}

// GetRun returns a stored run with its side-effect status.
func (c *PGCommitter) GetRun(ctx context.Context, actionRID string) (*RunRecord, error) {
	var rr RunRecord
	var result, errJSON []byte
	err := c.Pool.QueryRow(ctx, `SELECT action_rid, action_type, actor_id, COALESCE(on_behalf_of, ''), status, result, error
FROM edits.action_run WHERE action_rid = $1`, actionRID).Scan(&rr.ActionRID, &rr.ActionType, &rr.ActorID, &rr.OnBehalfOf,
		&rr.Status, &result, &errJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		rr.Result = &ApplyResult{}
		if err := json.Unmarshal(result, rr.Result); err != nil {
			return nil, err
		}
	}
	if len(errJSON) > 0 {
		_ = json.Unmarshal(errJSON, &rr.Error)
	}
	rows, err := c.Pool.Query(ctx, `SELECT kind, name, mode, status, attempt FROM edits.side_effect_run
WHERE action_rid = $1 ORDER BY id`, actionRID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rr.SideEffects = []SideEffectRecord{}
	for rows.Next() {
		var s SideEffectRecord
		if err := rows.Scan(&s.Kind, &s.Name, &s.Mode, &s.Status, &s.Attempt); err != nil {
			return nil, err
		}
		rr.SideEffects = append(rr.SideEffects, s)
	}
	return &rr, rows.Err()
}
