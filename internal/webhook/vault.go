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
	"os"
	"strings"
	"sync"
	"time"
)

// VaultSecrets resolves vault://<path>[#<field>] from a HashiCorp Vault KV v2 engine.
//
//   - vault://mro/client#token reads field "token" of secret mro/client.
//   - vault://mro/client returns field "value" when the secret has only that field, otherwise the
//     whole secret as a JSON object (the form the oauth2_client_credentials auth type expects).
//
// It logs in with a static token or with AppRole, and caches each secret for CacheTTL.
type VaultSecrets struct {
	// Addr is the Vault address, e.g. https://vault.internal:8200.
	Addr string
	// Mount is the KV v2 mount (default "secret").
	Mount string
	// Namespace is the Vault Enterprise namespace, optional.
	Namespace string
	// Token is a static token; ignored when RoleID is set.
	Token string
	// RoleID and SecretID log in with AppRole.
	RoleID   string
	SecretID string
	// CacheTTL is how long a secret is cached (default 5m; negative disables caching).
	CacheTTL time.Duration
	Client   *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	cache    map[string]cachedSecret
	now      func() time.Time
}

type cachedSecret struct {
	data map[string]any
	exp  time.Time
}

// VaultFromEnv configures Vault from VAULT_ADDR, VAULT_TOKEN, VAULT_ROLE_ID, VAULT_SECRET_ID,
// VAULT_NAMESPACE and VAULT_KV_MOUNT. It returns nil when VAULT_ADDR is unset.
func VaultFromEnv() (*VaultSecrets, error) {
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" {
		return nil, nil
	}
	v := &VaultSecrets{Addr: addr, Mount: os.Getenv("VAULT_KV_MOUNT"), Namespace: os.Getenv("VAULT_NAMESPACE"),
		Token: os.Getenv("VAULT_TOKEN"), RoleID: os.Getenv("VAULT_ROLE_ID"), SecretID: os.Getenv("VAULT_SECRET_ID")}
	if v.Token == "" && (v.RoleID == "" || v.SecretID == "") {
		return nil, errors.New("VAULT_ADDR is set: also set VAULT_TOKEN, or VAULT_ROLE_ID and VAULT_SECRET_ID")
	}
	return v, nil
}

// Secret implements SecretProvider.
func (v *VaultSecrets) Secret(ctx context.Context, ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, "vault://")
	if !ok {
		return "", fmt.Errorf("unsupported secret reference %q", ref)
	}
	path, field, _ := strings.Cut(rest, "#")
	path = strings.Trim(path, "/")
	if path == "" || strings.Contains(path, "..") {
		return "", fmt.Errorf("invalid vault secret reference %q", ref)
	}
	data, err := v.read(ctx, path)
	if err != nil {
		return "", fmt.Errorf("vault %s: %w", path, err)
	}
	if field == "" {
		if s, ok := data["value"].(string); ok && len(data) == 1 {
			return s, nil
		}
		b, err := json.Marshal(data)
		return string(b), err
	}
	val, ok := data[field]
	if !ok {
		return "", fmt.Errorf("vault %s has no field %q", path, field)
	}
	if s, ok := val.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(val)
	return string(b), err
}

func (v *VaultSecrets) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func (v *VaultSecrets) read(ctx context.Context, path string) (map[string]any, error) {
	ttl := v.CacheTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	v.mu.Lock()
	if c, ok := v.cache[path]; ok && v.clock().Before(c.exp) {
		v.mu.Unlock()
		return c.data, nil
	}
	v.mu.Unlock()

	data, status, err := v.fetch(ctx, path)
	if status == http.StatusForbidden && v.RoleID != "" {
		// The AppRole token may have been revoked or expired early: log in again once.
		v.mu.Lock()
		v.token = ""
		v.mu.Unlock()
		data, _, err = v.fetch(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	if ttl > 0 {
		v.mu.Lock()
		if v.cache == nil {
			v.cache = map[string]cachedSecret{}
		}
		v.cache[path] = cachedSecret{data: data, exp: v.clock().Add(ttl)}
		v.mu.Unlock()
	}
	return data, nil
}

func (v *VaultSecrets) fetch(ctx context.Context, path string) (map[string]any, int, error) {
	tok, err := v.clientToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	mount := v.Mount
	if mount == "" {
		mount = "secret"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := strings.TrimRight(v.Addr, "/") + "/v1/" + url.PathEscape(mount) + "/data/" + strings.Join(segs, "/")
	var body struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	status, err := v.do(ctx, http.MethodGet, u, tok, nil, &body)
	if err != nil {
		return nil, status, err
	}
	if body.Data.Data == nil {
		return nil, status, errors.New("secret not found or has no data")
	}
	return body.Data.Data, status, nil
}

func (v *VaultSecrets) clientToken(ctx context.Context) (string, error) {
	if v.RoleID == "" {
		if v.Token == "" {
			return "", errors.New("no vault token configured")
		}
		return v.Token, nil
	}
	v.mu.Lock()
	if v.token != "" && v.clock().Before(v.tokenExp) {
		t := v.token
		v.mu.Unlock()
		return t, nil
	}
	v.mu.Unlock()
	var body struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	login, _ := json.Marshal(map[string]string{"role_id": v.RoleID, "secret_id": v.SecretID})
	if _, err := v.do(ctx, http.MethodPost, strings.TrimRight(v.Addr, "/")+"/v1/auth/approle/login", "", login, &body); err != nil {
		return "", fmt.Errorf("approle login: %w", err)
	}
	if body.Auth.ClientToken == "" {
		return "", errors.New("approle login returned no token")
	}
	lease := time.Duration(body.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		lease = time.Hour
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.token = body.Auth.ClientToken
	v.tokenExp = v.clock().Add(lease * 9 / 10) // renew a little before the lease ends
	return v.token, nil
}

func (v *VaultSecrets) do(ctx context.Context, method, u, token string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Vault error bodies do not contain secret values; include the status only.
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("invalid vault response: %v", err)
	}
	return resp.StatusCode, nil
}

// Secrets routes a reference to the provider for its scheme (env:// and vault://).
type Secrets struct {
	Env SecretProvider
	// Vault resolves vault:// references; nil makes them fail with a clear error.
	Vault SecretProvider
}

// Secret implements SecretProvider.
func (s Secrets) Secret(ctx context.Context, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env://"):
		env := s.Env
		if env == nil {
			env = EnvSecrets{}
		}
		return env.Secret(ctx, ref)
	case strings.HasPrefix(ref, "vault://"):
		if s.Vault == nil {
			return "", fmt.Errorf("vault is not configured (set VAULT_ADDR) for %s", ref)
		}
		return s.Vault.Secret(ctx, ref)
	}
	return "", fmt.Errorf("unsupported secret reference %q", ref)
}
