// Package httpx holds small HTTP helpers shared by the services.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/acme/ontology/internal/platform/apperr"
)

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError maps err to its Error and writes it.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	ae := apperr.As(err)
	if ae.HTTPStatus >= 500 {
		slog.ErrorContext(r.Context(), "request failed", "code", ae.Code, "err", ae.Error(), "path", r.URL.Path)
	}
	WriteJSON(w, ae.HTTPStatus, ae)
}

// DecodeJSON decodes the request body into v, rejecting unknown fields.
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("INVALID_BODY", "invalid request body: %v", err)
	}
	return nil
}
