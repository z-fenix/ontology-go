package action

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/platform/httpx"
)

// IdentityFunc extracts the caller from a request. Production MUST verify an OIDC JWT
// (Spec §8.1); HeaderIdentity is for local development only.
type IdentityFunc func(r *http.Request) (Actor, error)

// HeaderIdentity trusts X-Actor / X-Actor-Kind / X-On-Behalf-Of / X-Actor-Groups. Dev only.
func HeaderIdentity(r *http.Request) (Actor, error) {
	a := Actor{ID: r.Header.Get("X-Actor"), Kind: ActorKind(strings.ToUpper(r.Header.Get("X-Actor-Kind"))),
		OnBehalfOf: r.Header.Get("X-On-Behalf-Of")}
	if a.ID == "" {
		return a, apperr.New(http.StatusUnauthorized, "UNAUTHENTICATED", "missing X-Actor header")
	}
	if a.Kind == "" {
		a.Kind = ActorUser
	}
	if a.Kind != ActorUser && a.Kind != ActorAgent && a.Kind != ActorService {
		return a, apperr.BadRequest("INVALID_ACTOR", "X-Actor-Kind must be USER, AGENT or SERVICE")
	}
	if g := r.Header.Get("X-Actor-Groups"); g != "" {
		a.Groups = strings.Split(g, ",")
	}
	return a, nil
}

// Handler exposes the Action Service HTTP API (Spec §7.8).
type Handler struct {
	Engine    *Engine
	Committer Committer
	Ontology  string // the ontology apiName this service instance serves
	Identity  IdentityFunc
	MaxBatch  int
}

// Routes mounts the endpoints.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Route("/api/v1/ontologies/{ont}", func(r chi.Router) {
		r.Post("/actions/{action}/validate", h.run(ModeValidate))
		r.Post("/actions/{action}/apply", h.run(ModeApply))
		r.Post("/actions/{action}/applyBatch", h.applyBatch)
		r.Get("/actionRuns/{rid}", h.getRun)
	})
	return r
}

type applyBody struct {
	Parameters      map[string]any `json:"parameters"`
	OntologyVersion *int64         `json:"ontologyVersion,omitempty"`
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20)); err != nil {
		return apperr.BadRequest("INVALID_BODY", "cannot read body: %v", err)
	}
	dec := json.NewDecoder(&buf)
	dec.UseNumber() // keep integer precision; coerce() understands json.Number
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("INVALID_BODY", "invalid request body: %v", err)
	}
	return nil
}

func (h *Handler) prepare(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	if ont := chi.URLParam(r, "ont"); ont != h.Ontology {
		httpx.WriteError(w, r, apperr.New(http.StatusNotFound, "ONTOLOGY_NOT_FOUND", "ontology %q is not served here", ont))
		return Actor{}, false
	}
	actor, err := h.Identity(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return Actor{}, false
	}
	return actor, true
}

func (h *Handler) run(mode Mode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.prepare(w, r)
		if !ok {
			return
		}
		var body applyBody
		if err := decode(w, r, &body); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if body.Parameters == nil {
			body.Parameters = map[string]any{}
		}
		res, err := h.Engine.Apply(r.Context(), ApplyRequest{
			ActionType: chi.URLParam(r, "action"), Parameters: body.Parameters, OntologyVersion: body.OntologyVersion,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor, Mode: mode,
		})
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}

type batchItem struct {
	Index  int           `json:"index"`
	Result *ApplyResult  `json:"result,omitempty"`
	Error  *apperr.Error `json:"error,omitempty"`
}

// applyBatch runs each request in its own transaction (Spec §7.8); failures do not stop the batch.
func (h *Handler) applyBatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.prepare(w, r)
	if !ok {
		return
	}
	var body struct {
		Requests []applyBody `json:"requests"`
	}
	if err := decode(w, r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	max := h.MaxBatch
	if max <= 0 {
		max = 100
	}
	if len(body.Requests) == 0 || len(body.Requests) > max {
		httpx.WriteError(w, r, apperr.BadRequest("INVALID_BATCH", "a batch must contain 1..%d requests", max))
		return
	}
	out := make([]batchItem, 0, len(body.Requests))
	for i, item := range body.Requests {
		if item.Parameters == nil {
			item.Parameters = map[string]any{}
		}
		res, err := h.Engine.Apply(r.Context(), ApplyRequest{ActionType: chi.URLParam(r, "action"),
			Parameters: item.Parameters, OntologyVersion: item.OntologyVersion, Actor: actor, Mode: ModeApply})
		bi := batchItem{Index: i, Result: res}
		if err != nil {
			bi.Error = apperr.As(err)
		}
		out = append(out, bi)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}

// getRun returns a run to its actor only (owner-wide audit queries come with OIDC roles).
func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.prepare(w, r)
	if !ok {
		return
	}
	rr, err := h.Committer.GetRun(r.Context(), chi.URLParam(r, "rid"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if rr == nil || rr.ActorID != actor.ID {
		httpx.WriteError(w, r, apperr.New(http.StatusNotFound, "ACTION_RUN_NOT_FOUND", "action run not found"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rr)
}
