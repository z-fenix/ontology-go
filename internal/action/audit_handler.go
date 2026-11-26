package action

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/platform/httpx"
)

// isAuditor reports whether the actor may read every run (member of an AuditGroups group).
func (h *Handler) isAuditor(a Actor) bool {
	for _, g := range a.Groups {
		for _, ag := range h.AuditGroups {
			if g == ag {
				return true
			}
		}
	}
	return false
}

// canSeeRun: auditors see all runs; others see runs they made, or that an agent made for them.
func (h *Handler) canSeeRun(a Actor, actorID, onBehalfOf string) bool {
	return h.isAuditor(a) || actorID == a.ID || (onBehalfOf != "" && onBehalfOf == a.ID)
}

func encodeCursor(c *RunCursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*RunCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c RunCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ActionRID == "" {
		return nil, apperr.BadRequest("INVALID_PAGE_TOKEN", "invalid pageToken")
	}
	return &c, nil
}

func parseTime(q, name string) (time.Time, error) {
	if q == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, q)
	if err != nil {
		return t, apperr.BadRequest("INVALID_QUERY", "%s must be an RFC 3339 timestamp", name)
	}
	return t, nil
}

// listRuns serves GET /actionRuns?actor=&actionType=&status=&objectType=&objectPk=&since=&until=&pageSize=&pageToken=
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.prepare(w, r)
	if !ok {
		return
	}
	if h.Audit == nil {
		httpx.WriteError(w, r, apperr.New(http.StatusNotImplemented, "AUDIT_UNAVAILABLE", "audit queries are not configured"))
		return
	}
	q := r.URL.Query()
	f := RunFilter{Ontology: h.Ontology, ActorID: q.Get("actor"), ActionType: q.Get("actionType"), Status: q.Get("status")}
	var err error
	if f.Since, err = parseTime(q.Get("since"), "since"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if f.Until, err = parseTime(q.Get("until"), "until"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if ot, pk := q.Get("objectType"), q.Get("objectPk"); ot != "" || pk != "" {
		if ot == "" || pk == "" {
			httpx.WriteError(w, r, apperr.BadRequest("INVALID_QUERY", "objectType and objectPk go together"))
			return
		}
		f.Object = &objstore.ObjectRef{Type: ot, PK: pk}
	}
	if ps := q.Get("pageSize"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > MaxAuditPage {
			httpx.WriteError(w, r, apperr.BadRequest("INVALID_QUERY", "pageSize must be 1..%d", MaxAuditPage))
			return
		}
		f.Limit = n
	}
	if tok := q.Get("pageToken"); tok != "" {
		if f.After, err = decodeCursor(tok); err != nil {
			httpx.WriteError(w, r, apperr.BadRequest("INVALID_PAGE_TOKEN", "invalid pageToken"))
			return
		}
	}
	// Non-auditors only ever see their own runs, whatever the filter says.
	if !h.isAuditor(actor) {
		if f.ActorID != "" && f.ActorID != actor.ID {
			httpx.WriteError(w, r, apperr.New(http.StatusForbidden, "PERMISSION_DENIED", "only auditors can list other actors' runs"))
			return
		}
		f.ActorID = actor.ID
	}
	runs, next, err := h.Audit.ListRuns(r.Context(), f)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": runs, "nextPageToken": encodeCursor(next)})
}

// getRunEdits serves GET /actionRuns/{rid}/edits.
func (h *Handler) getRunEdits(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.prepare(w, r)
	if !ok {
		return
	}
	if h.Audit == nil {
		httpx.WriteError(w, r, apperr.New(http.StatusNotImplemented, "AUDIT_UNAVAILABLE", "audit queries are not configured"))
		return
	}
	rid := chi.URLParam(r, "rid")
	rr, err := h.Committer.GetRun(r.Context(), rid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if rr == nil || !h.canSeeRun(actor, rr.ActorID, rr.OnBehalfOf) {
		httpx.WriteError(w, r, apperr.New(http.StatusNotFound, "ACTION_RUN_NOT_FOUND", "action run not found"))
		return
	}
	edits, err := h.Audit.ListEdits(r.Context(), rid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionRid": rid, "edits": edits})
}
