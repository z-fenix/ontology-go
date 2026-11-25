package query

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/platform/httpx"
)

// Handler exposes the query API under /api/v1/ontologies/{ont}.
type Handler struct {
	Service  *Service
	Ontology string // the ontology apiName this instance serves
	Auth     auth.Authenticator
}

// Routes mounts the endpoints.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Route("/api/v1/ontologies/{ont}/objects/{type}", func(r chi.Router) {
		r.Post("/search", h.search)
		r.Post("/aggregate", h.aggregate)
		r.Get("/{pk}", h.get)
		r.Get("/{pk}/links/{link}", h.links)
	})
	return r
}

func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	if ont := chi.URLParam(r, "ont"); ont != h.Ontology {
		httpx.WriteError(w, r, apperr.New(http.StatusNotFound, "ONTOLOGY_NOT_FOUND", "ontology %q is not served here", ont))
		return Caller{}, false
	}
	p, err := h.Auth.Authenticate(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return Caller{}, false
	}
	return Caller{ID: p.ID, Kind: p.Kind, OnBehalfOf: p.OnBehalfOf, Groups: p.Groups}, true
}

func versionParam(r *http.Request) (*int64, error) {
	s := r.URL.Query().Get("ontologyVersion")
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, apperr.BadRequest("INVALID_VERSION", "ontologyVersion must be an integer")
	}
	return &v, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20)); err != nil {
		return apperr.BadRequest("INVALID_BODY", "cannot read body: %v", err)
	}
	if buf.Len() == 0 {
		return nil // an empty body means "no filter"
	}
	dec := json.NewDecoder(&buf)
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("INVALID_BODY", "invalid request body: %v", err)
	}
	return nil
}

func setVersion(w http.ResponseWriter, v int64) {
	w.Header().Set("X-Ontology-Version", strconv.FormatInt(v, 10))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	v, err := versionParam(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	obj, ver, err := h.Service.Get(r.Context(), c, v, chi.URLParam(r, "type"), chi.URLParam(r, "pk"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersion(w, ver)
	httpx.WriteJSON(w, http.StatusOK, obj)
}

func (h *Handler) links(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	v, err := versionParam(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	page, err := h.Service.Links(r.Context(), c, v, chi.URLParam(r, "type"), chi.URLParam(r, "pk"), chi.URLParam(r, "link"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersion(w, page.OntologyVersion)
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req SearchRequest
	if err := decode(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	page, err := h.Service.Search(r.Context(), c, chi.URLParam(r, "type"), req)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersion(w, page.OntologyVersion)
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request) {
	c, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req AggregateRequest
	if err := decode(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Service.Aggregate(r.Context(), c, chi.URLParam(r, "type"), req)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	setVersion(w, res.OntologyVersion)
	httpx.WriteJSON(w, http.StatusOK, res)
}
