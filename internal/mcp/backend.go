package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// forwardedHeaders carry the caller's identity to the backend services, which perform all
// authentication and authorization. Authorization holds an OIDC bearer token in production;
// the X-Actor headers are honored only by backends running with ONTO_AUTH_DEV_HEADERS.
var forwardedHeaders = []string{"Authorization", "X-Actor", "X-Actor-Kind", "X-On-Behalf-Of", "X-Actor-Groups"}

// Backend calls query-service and action-service on behalf of the MCP caller.
type Backend struct {
	QueryURL  string
	ActionURL string
	Ontology  string
	HTTP      *http.Client
}

// BackendError is a non-2xx response from a backend service.
type BackendError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *BackendError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("backend returned %d: %s", e.Status, e.Message)
}

func (b *Backend) call(ctx context.Context, from http.Header, method, base, path string, body any, extra map[string]string) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range forwardedHeaders {
		if v := from.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend unavailable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		be := &BackendError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		if out != nil {
			be.Code, _ = out["errorCode"].(string)
			if m, ok := out["message"].(string); ok {
				be.Message = m
			}
			be.Details, _ = out["details"].(map[string]any)
		}
		return nil, be
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func (b *Backend) objectsPath(parts ...string) string {
	p := "/api/v1/ontologies/" + url.PathEscape(b.Ontology) + "/objects"
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

func (b *Backend) actionPath(action, op string) string {
	return "/api/v1/ontologies/" + url.PathEscape(b.Ontology) + "/actions/" + url.PathEscape(action) + "/" + op
}

// GetObject reads one object.
func (b *Backend) GetObject(ctx context.Context, h http.Header, typ, pk string) (map[string]any, error) {
	return b.call(ctx, h, http.MethodGet, b.QueryURL, b.objectsPath(typ, pk), nil, nil)
}

// Links follows a link from an object.
func (b *Backend) Links(ctx context.Context, h http.Header, typ, pk, link string, pageSize int) (map[string]any, error) {
	p := b.objectsPath(typ, pk, "links", link)
	if pageSize > 0 {
		p += fmt.Sprintf("?pageSize=%d", pageSize)
	}
	return b.call(ctx, h, http.MethodGet, b.QueryURL, p, nil, nil)
}

// Search runs an object-set search; body follows the query-service request format.
func (b *Backend) Search(ctx context.Context, h http.Header, typ string, body map[string]any) (map[string]any, error) {
	return b.call(ctx, h, http.MethodPost, b.QueryURL, b.objectsPath(typ, "search"), body, nil)
}

// Aggregate runs an aggregation.
func (b *Backend) Aggregate(ctx context.Context, h http.Header, typ string, body map[string]any) (map[string]any, error) {
	return b.call(ctx, h, http.MethodPost, b.QueryURL, b.objectsPath(typ, "aggregate"), body, nil)
}

// Validate checks an action's parameters and submission criteria without writing anything.
func (b *Backend) Validate(ctx context.Context, h http.Header, action string, params map[string]any) (map[string]any, error) {
	return b.call(ctx, h, http.MethodPost, b.ActionURL, b.actionPath(action, "validate"), map[string]any{"parameters": params}, nil)
}

// Apply executes an action. idemKey, when set, makes retries of the same call safe.
func (b *Backend) Apply(ctx context.Context, h http.Header, action string, params map[string]any, idemKey string) (map[string]any, error) {
	extra := map[string]string{}
	if idemKey != "" {
		extra["Idempotency-Key"] = idemKey
	}
	return b.call(ctx, h, http.MethodPost, b.ActionURL, b.actionPath(action, "apply"), map[string]any{"parameters": params}, extra)
}
