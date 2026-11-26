package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/acme/ontology/internal/objstore"
)

// RunFilter selects action runs for the audit API. Zero fields do not filter.
type RunFilter struct {
	Ontology   string
	ActorID    string
	ActionType string
	Status     string
	Object     *objstore.ObjectRef
	Since      time.Time
	Until      time.Time
	Limit      int
	After      *RunCursor
}

// RunCursor is the keyset position of the last run on a page (newest first).
type RunCursor struct {
	CreatedAt time.Time `json:"t"`
	ActionRID string    `json:"r"`
}

// RunSummary is one action run as listed by the audit API.
type RunSummary struct {
	ActionRID       string         `json:"actionRid"`
	ActionType      string         `json:"actionType"`
	OntologyVersion int64          `json:"ontologyVersion"`
	ActorID         string         `json:"actor"`
	ActorKind       string         `json:"actorKind"`
	OnBehalfOf      string         `json:"onBehalfOf,omitempty"`
	Status          string         `json:"status"`
	Error           map[string]any `json:"error,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	CommittedAt     *time.Time     `json:"committedAt,omitempty"`
}

// EditRecord is one stored object edit of a run.
type EditRecord struct {
	EditID     int64          `json:"editId"`
	ObjectType string         `json:"objectType"`
	ObjectPK   string         `json:"objectPk"`
	Op         string         `json:"op"`
	LinkType   string         `json:"linkType,omitempty"`
	Changes    map[string]any `json:"changes"`
	Before     map[string]any `json:"before,omitempty"`
	EditedAt   time.Time      `json:"editedAt"`
}

// AuditStore answers audit queries (implemented by PGCommitter).
type AuditStore interface {
	// ListRuns returns runs newest first; the cursor is non-nil when more runs follow.
	ListRuns(ctx context.Context, f RunFilter) ([]RunSummary, *RunCursor, error)
	ListEdits(ctx context.Context, actionRID string) ([]EditRecord, error)
}

var _ AuditStore = (*PGCommitter)(nil)

// MaxAuditPage caps RunFilter.Limit.
const MaxAuditPage = 200

// ListRuns implements AuditStore with keyset pagination on (created_at, action_rid).
func (c *PGCommitter) ListRuns(ctx context.Context, f RunFilter) ([]RunSummary, *RunCursor, error) {
	limit := f.Limit
	if limit <= 0 || limit > MaxAuditPage {
		limit = 50
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Ontology != "" {
		add("r.ontology = $%d", f.Ontology)
	}
	if f.ActorID != "" {
		add("r.actor_id = $%d", f.ActorID)
	}
	if f.ActionType != "" {
		add("r.action_type = $%d", f.ActionType)
	}
	if f.Status != "" {
		add("r.status = $%d", f.Status)
	}
	if !f.Since.IsZero() {
		add("r.created_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("r.created_at < $%d", f.Until)
	}
	if f.Object != nil {
		args = append(args, f.Object.Type, f.Object.PK)
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM edits.object_edit e
  WHERE e.action_rid = r.action_rid AND e.object_type = $%d AND e.object_pk = $%d)`, len(args)-1, len(args)))
	}
	if f.After != nil {
		args = append(args, f.After.CreatedAt, f.After.ActionRID)
		where = append(where, fmt.Sprintf("(r.created_at, r.action_rid) < ($%d, $%d)", len(args)-1, len(args)))
	}
	q := `SELECT r.action_rid, r.action_type, r.ontology_ver, r.actor_id, r.actor_kind, COALESCE(r.on_behalf_of, ''),
  r.status, r.error, r.created_at, r.committed_at FROM edits.action_run r`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit+1)
	q += fmt.Sprintf(" ORDER BY r.created_at DESC, r.action_rid DESC LIMIT $%d", len(args))

	rows, err := c.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []RunSummary{}
	for rows.Next() {
		var s RunSummary
		var errJSON []byte
		if err := rows.Scan(&s.ActionRID, &s.ActionType, &s.OntologyVersion, &s.ActorID, &s.ActorKind, &s.OnBehalfOf,
			&s.Status, &errJSON, &s.CreatedAt, &s.CommittedAt); err != nil {
			return nil, nil, err
		}
		if len(errJSON) > 0 {
			_ = json.Unmarshal(errJSON, &s.Error)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) <= limit {
		return out, nil, nil
	}
	out = out[:limit]
	last := out[limit-1]
	return out, &RunCursor{CreatedAt: last.CreatedAt, ActionRID: last.ActionRID}, nil
}

// ListEdits implements AuditStore.
func (c *PGCommitter) ListEdits(ctx context.Context, actionRID string) ([]EditRecord, error) {
	rows, err := c.Pool.Query(ctx, `SELECT edit_id, object_type, object_pk, op, COALESCE(link_type, ''), changes, before, edit_ts
FROM edits.object_edit WHERE action_rid = $1 ORDER BY edit_id`, actionRID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EditRecord{}
	for rows.Next() {
		var e EditRecord
		var changes, before []byte
		if err := rows.Scan(&e.EditID, &e.ObjectType, &e.ObjectPK, &e.Op, &e.LinkType, &changes, &before, &e.EditedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(changes, &e.Changes); err != nil {
			return nil, err
		}
		if len(before) > 0 {
			if err := json.Unmarshal(before, &e.Before); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
