package oms

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/platform/httpx"
	"github.com/acme/ontology/internal/spec"
)

// Handler exposes the OMS HTTP API (Spec §5.5).
type Handler struct {
	svc   *Service
	store *Store
}

// NewHandler builds the OMS handler.
func NewHandler(svc *Service, store *Store) *Handler { return &Handler{svc: svc, store: store} }

// Routes mounts the OMS endpoints under /api/v1.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Route("/api/v1/ontologies/{ont}", func(r chi.Router) {
		r.Post("/versions", h.publish)
		r.Get("/versions", h.listVersions)
		r.Get("/versions/{v}", h.getVersion)
		r.Post("/versions/{v}/activate", h.activate)
		r.Get("/compiled", h.getCompiled)
		r.Get("/objectTypes", h.listObjectTypes)
		r.Get("/objectTypes/{apiName}", h.getObjectType)
		r.Get("/actionTypes", h.listActionTypes)
		r.Get("/actionTypes/{apiName}", h.getActionType)
	})
	return r
}

func actor(r *http.Request) string {
	if a := r.Header.Get("X-Actor"); a != "" {
		return a
	}
	return "unknown"
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	var in PublishInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	in.Ontology = chi.URLParam(r, "ont")
	in.Actor = actor(r)
	res, err := h.svc.Publish(r.Context(), in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Reused {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, res)
}

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request) {
	vers, err := h.store.ListVersions(r.Context(), chi.URLParam(r, "ont"), 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"versions": vers})
}

func (h *Handler) getVersion(w http.ResponseWriter, r *http.Request) {
	v, err := parseVersion(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ont := chi.URLParam(r, "ont")
	c, err := h.store.GetCompiled(r.Context(), ont, v)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if c == nil {
		httpx.WriteError(w, r, apperr.NotFound("version %d not found", v))
		return
	}
	setVersionHeader(w, c.Version)
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	v, err := parseVersion(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.svc.ActivateVersion(r.Context(), chi.URLParam(r, "ont"), v); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ontology": chi.URLParam(r, "ont"), "activeVersion": v})
}

func (h *Handler) loadForRead(r *http.Request) (*spec.CompiledOntology, error) {
	ont := chi.URLParam(r, "ont")
	if vs := r.URL.Query().Get("version"); vs != "" {
		v, err := strconv.ParseInt(vs, 10, 64)
		if err != nil {
			return nil, apperr.BadRequest("INVALID_VERSION", "version must be an integer")
		}
		c, err := h.store.GetCompiled(r.Context(), ont, v)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, apperr.NotFound("version %d not found", v)
		}
		return c, nil
	}
	c, err := h.store.GetActiveCompiled(r.Context(), ont)
	if errors.Is(err, ErrNoActive) {
		return nil, apperr.NotFound("ontology %q has no active version", ont)
	}
	return c, err
}

func (h *Handler) getCompiled(w http.ResponseWriter, r *http.Request) {
	c, err := h.loadForRead(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersionHeader(w, c.Version)
	w.Header().Set("ETag", `"`+c.Checksum+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+c.Checksum+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) listObjectTypes(w http.ResponseWriter, r *http.Request) {
	c, err := h.loadForRead(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersionHeader(w, c.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"objectTypes": c.ObjectTypes})
}

func (h *Handler) getObjectType(w http.ResponseWriter, r *http.Request) {
	c, err := h.loadForRead(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ot, ok := c.ObjectTypes[chi.URLParam(r, "apiName")]
	if !ok {
		httpx.WriteError(w, r, apperr.NotFound("object type %q not found", chi.URLParam(r, "apiName")))
		return
	}
	setVersionHeader(w, c.Version)
	httpx.WriteJSON(w, http.StatusOK, ot)
}

func (h *Handler) listActionTypes(w http.ResponseWriter, r *http.Request) {
	c, err := h.loadForRead(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersionHeader(w, c.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionTypes": c.ActionTypes})
}

func (h *Handler) getActionType(w http.ResponseWriter, r *http.Request) {
	c, err := h.loadForRead(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	at, ok := c.ActionTypes[chi.URLParam(r, "apiName")]
	if !ok {
		httpx.WriteError(w, r, apperr.NotFound("action type %q not found", chi.URLParam(r, "apiName")))
		return
	}
	setVersionHeader(w, c.Version)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actionType": at, "parametersSchema": paramsSchema(at)})
}

func parseVersion(r *http.Request) (int64, error) {
	v, err := strconv.ParseInt(chi.URLParam(r, "v"), 10, 64)
	if err != nil {
		return 0, apperr.BadRequest("INVALID_VERSION", "version must be an integer")
	}
	return v, nil
}

func setVersionHeader(w http.ResponseWriter, v int64) {
	w.Header().Set("X-Ontology-Version", strconv.FormatInt(v, 10))
}

// paramsSchema renders an action type's parameters as a JSON-Schema-ish object (Spec §5.5).
func paramsSchema(at *spec.ActionType) map[string]any {
	props := map[string]any{}
	var required []string
	for _, name := range at.ParamOrder {
		p := at.Parameters[name]
		entry := map[string]any{}
		switch p.Type.Kind {
		case spec.KindObjectRef:
			entry["type"] = "string"
			entry["x-object-type"] = p.Type.ObjectType
		case spec.KindObjectRefList:
			entry["type"] = "array"
			entry["x-object-type"] = p.Type.ObjectType
		default:
			entry["type"] = jsonType(p.Type.Base)
		}
		props[name] = entry
		if p.Required {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func jsonType(b spec.BaseType) string {
	switch b {
	case spec.TInteger, spec.TLong:
		return "integer"
	case spec.TDouble, spec.TDecimal:
		return "number"
	case spec.TBoolean:
		return "boolean"
	}
	return "string"
}
