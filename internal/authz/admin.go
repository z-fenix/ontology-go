package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Admin manages OpenFGA stores and authorization models (used by `ontoctl fga`).
type Admin struct {
	APIURL string
	Token  string // optional pre-shared key / bearer token
	HTTP   *http.Client
}

func (a *Admin) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.APIURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	client := a.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openfga %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Store is an OpenFGA store.
type Store struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindStore returns the first store named name, or nil.
func (a *Admin) FindStore(ctx context.Context, name string) (*Store, error) {
	token := ""
	for {
		path := "/stores?page_size=100"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		var resp struct {
			Stores            []Store `json:"stores"`
			ContinuationToken string  `json:"continuation_token"`
		}
		if err := a.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Stores {
			if s.Name == name {
				st := s
				return &st, nil
			}
		}
		if resp.ContinuationToken == "" {
			return nil, nil
		}
		token = resp.ContinuationToken
	}
}

// EnsureStore returns the store named name, creating it when missing.
func (a *Admin) EnsureStore(ctx context.Context, name string) (*Store, bool, error) {
	if s, err := a.FindStore(ctx, name); err != nil || s != nil {
		return s, false, err
	}
	var s Store
	if err := a.do(ctx, http.MethodPost, "/stores", map[string]string{"name": name}, &s); err != nil {
		return nil, false, err
	}
	return &s, true, nil
}

// LatestModel returns the newest model of a store (raw JSON) and its ID, or "" when none exists.
func (a *Admin) LatestModel(ctx context.Context, storeID string) (string, json.RawMessage, error) {
	var resp struct {
		AuthorizationModels []json.RawMessage `json:"authorization_models"`
	}
	if err := a.do(ctx, http.MethodGet, "/stores/"+storeID+"/authorization-models?page_size=1", nil, &resp); err != nil {
		return "", nil, err
	}
	if len(resp.AuthorizationModels) == 0 {
		return "", nil, nil
	}
	var head struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp.AuthorizationModels[0], &head)
	return head.ID, resp.AuthorizationModels[0], nil
}

// canonical returns a comparable form of a model: schema version plus sorted type definitions.
func canonical(raw []byte) (string, error) {
	var m struct {
		SchemaVersion   string            `json:"schema_version"`
		TypeDefinitions []json.RawMessage `json:"type_definitions"`
		Conditions      json.RawMessage   `json:"conditions,omitempty"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	defs := make([]string, 0, len(m.TypeDefinitions))
	for _, d := range m.TypeDefinitions {
		var v any
		if err := json.Unmarshal(d, &v); err != nil {
			return "", err
		}
		b, _ := json.Marshal(v) // re-encoding sorts object keys
		defs = append(defs, string(b))
	}
	sort.Strings(defs)
	cond := ""
	if len(m.Conditions) > 0 && string(m.Conditions) != "{}" && string(m.Conditions) != "null" {
		var v any
		_ = json.Unmarshal(m.Conditions, &v)
		b, _ := json.Marshal(v)
		cond = string(b)
	}
	return m.SchemaVersion + "|" + strings.Join(defs, "|") + "|" + cond, nil
}

// EnsureModel writes model to the store unless the latest model is already identical. It
// returns the model ID to configure (ONTO_FGA_MODEL_ID) and whether a new one was written.
func (a *Admin) EnsureModel(ctx context.Context, storeID string, model []byte) (string, bool, error) {
	want, err := canonical(model)
	if err != nil {
		return "", false, fmt.Errorf("authorization model: %w", err)
	}
	id, latest, err := a.LatestModel(ctx, storeID)
	if err != nil {
		return "", false, err
	}
	if latest != nil {
		if have, err := canonical(latest); err == nil && have == want {
			return id, false, nil
		}
	}
	var body map[string]any
	if err := json.Unmarshal(model, &body); err != nil {
		return "", false, err
	}
	var resp struct {
		AuthorizationModelID string `json:"authorization_model_id"`
	}
	if err := a.do(ctx, http.MethodPost, "/stores/"+storeID+"/authorization-models", body, &resp); err != nil {
		return "", false, err
	}
	return resp.AuthorizationModelID, true, nil
}
