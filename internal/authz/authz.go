// Package authz checks object-level permissions (Spec §8.2).
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnavailable means the authorization backend could not answer; callers MUST fail closed.
var ErrUnavailable = errors.New("authorization backend unavailable")

// Authorizer answers "may user have relation on object?".
// user and object use OpenFGA notation, e.g. "user:alice", "work_order:WO-1".
type Authorizer interface {
	Check(ctx context.Context, user, relation, object string) (bool, error)
}

// AllowAll permits everything. Local development only.
type AllowAll struct{}

// Check always allows.
func (AllowAll) Check(context.Context, string, string, string) (bool, error) { return true, nil }

// OpenFGA calls the OpenFGA HTTP Check API.
type OpenFGA struct {
	APIURL  string // e.g. http://openfga:8080
	StoreID string
	ModelID string // optional; empty uses the store's latest model
	HTTP    *http.Client
}

// NewOpenFGA builds a client with a short timeout.
func NewOpenFGA(apiURL, storeID, modelID string) *OpenFGA {
	return &OpenFGA{APIURL: apiURL, StoreID: storeID, ModelID: modelID, HTTP: &http.Client{Timeout: 3 * time.Second}}
}

type tupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type checkRequest struct {
	TupleKey             tupleKey `json:"tuple_key"`
	AuthorizationModelID string   `json:"authorization_model_id,omitempty"`
}

// Check calls POST /stores/{store_id}/check.
func (f *OpenFGA) Check(ctx context.Context, user, relation, object string) (bool, error) {
	body, err := json.Marshal(checkRequest{TupleKey: tupleKey{user, relation, object}, AuthorizationModelID: f.ModelID})
	if err != nil {
		return false, err
	}
	url := fmt.Sprintf("%s/stores/%s/check", f.APIURL, f.StoreID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("%w: openfga %s: %s", ErrUnavailable, resp.Status, msg)
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	return out.Allowed, nil
}
