// Package webhook executes outbound HTTP calls for action write-back and side effects (Spec §7.6).
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/acme/ontology/internal/spec"
)

// Request is a fully rendered webhook call (templates already evaluated by the action engine).
type Request struct {
	Name      string             `json:"name"`
	Method    string             `json:"method"`
	URL       string             `json:"url"`
	Headers   map[string]string  `json:"headers,omitempty"`
	Body      map[string]any     `json:"body,omitempty"`
	TimeoutMs int                `json:"timeoutMs"`
	Retry     spec.WebhookRetry  `json:"retry"`
	Auth      spec.WebhookAuth   `json:"auth"`
}

// Response is what the remote system returned (body truncated for audit storage).
type Response struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// Error classifies a failed call. Retryable covers network errors, 429 and 5xx; everything
// else (bad URL, disallowed host, missing secret, other 4xx, redirects) is permanent.
type Error struct {
	Status    int
	Retryable bool
	Msg       string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("webhook HTTP %d: %s", e.Status, e.Msg)
	}
	return "webhook: " + e.Msg
}

// IsPermanent reports whether err must not be retried.
func IsPermanent(err error) bool {
	var we *Error
	return errors.As(err, &we) && !we.Retryable
}

func permanent(status int, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Executor performs webhook calls.
type Executor struct {
	Secrets      SecretProvider
	AllowedHosts []string // exact hostnames, or "*.example.com" suffix patterns
	Client       *http.Client
	MaxBodyLog   int

	mu     sync.Mutex
	tokens map[string]oauth2.TokenSource
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewExecutor builds an executor that refuses redirects (a redirect could bypass the allowlist).
func NewExecutor(secrets SecretProvider, allowedHosts []string) *Executor {
	return &Executor{Secrets: secrets, AllowedHosts: allowedHosts, MaxBodyLog: 8192,
		Client: &http.Client{CheckRedirect: noRedirect}}
}

func (x *Executor) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, h := range x.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, "*.") {
			if strings.HasSuffix(host, h[1:]) {
				return true
			}
			continue
		}
		if host == h {
			return true
		}
	}
	return false
}

// Do sends req. idempotencyKey is forwarded as the Idempotency-Key header so the remote system
// can deduplicate retries.
func (x *Executor) Do(ctx context.Context, req Request, idempotencyKey string) (*Response, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, permanent(0, "invalid webhook URL %q", req.URL)
	}
	if !x.hostAllowed(u.Hostname()) {
		return nil, permanent(0, "host %q is not in the webhook allowlist", u.Hostname())
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, permanent(0, "encode body: %v", err)
		}
		body = bytes.NewReader(b)
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, permanent(0, "build request: %v", err)
	}
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	hreq.Header.Set("Accept", "application/json")
	if idempotencyKey != "" {
		hreq.Header.Set("Idempotency-Key", idempotencyKey)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	if err := x.authorize(ctx, hreq, req.Auth); err != nil {
		return nil, permanent(0, "auth: %v", err)
	}
	client := x.Client
	if client == nil {
		client = &http.Client{CheckRedirect: noRedirect}
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, &Error{Retryable: true, Msg: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	out := &Response{Status: resp.StatusCode, Body: truncate(string(raw), x.maxBody())}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return out, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return out, &Error{Status: resp.StatusCode, Retryable: true, Msg: out.Body}
	default:
		return out, &Error{Status: resp.StatusCode, Msg: out.Body}
	}
}

func (x *Executor) maxBody() int {
	if x.MaxBodyLog <= 0 {
		return 8192
	}
	return x.MaxBodyLog
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// authorize adds credentials. Secrets never appear in Response or in recorded requests.
func (x *Executor) authorize(ctx context.Context, r *http.Request, a spec.WebhookAuth) error {
	if a.Type == "" || a.Type == "none" {
		return nil
	}
	if x.Secrets == nil {
		return fmt.Errorf("no secret provider configured")
	}
	secret, err := x.Secrets.Secret(ctx, a.SecretRef)
	if err != nil {
		return err
	}
	switch a.Type {
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+secret)
	case "basic":
		user, pass, ok := strings.Cut(secret, ":")
		if !ok {
			return fmt.Errorf("basic auth secret must be user:password")
		}
		r.SetBasicAuth(user, pass)
	case "oauth2_client_credentials":
		tok, err := x.token(a.SecretRef, secret)
		if err != nil {
			return err
		}
		tok.SetAuthHeader(r)
	default:
		return fmt.Errorf("unsupported auth type %q", a.Type)
	}
	return nil
}

// token caches one token source per secret so tokens are reused until they expire.
func (x *Executor) token(ref, secret string) (*oauth2.Token, error) {
	x.mu.Lock()
	ts, ok := x.tokens[ref]
	if !ok {
		var cfg struct {
			ClientID     string   `json:"clientId"`
			ClientSecret string   `json:"clientSecret"`
			TokenURL     string   `json:"tokenUrl"`
			Scopes       []string `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(secret), &cfg); err != nil {
			x.mu.Unlock()
			return nil, fmt.Errorf("oauth2 secret must be JSON {clientId, clientSecret, tokenUrl}: %v", err)
		}
		cc := clientcredentials.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, TokenURL: cfg.TokenURL, Scopes: cfg.Scopes}
		ts = cc.TokenSource(context.Background())
		if x.tokens == nil {
			x.tokens = map[string]oauth2.TokenSource{}
		}
		x.tokens[ref] = ts
	}
	x.mu.Unlock()
	return ts.Token()
}
