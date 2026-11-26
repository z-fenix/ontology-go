package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeVault struct {
	logins, reads atomic.Int32
	revoked       atomic.Bool
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/approle/login":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != "role" || body["secret_id"] != "sid" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.logins.Add(1)
		f.revoked.Store(false)
		_, _ = w.Write([]byte(`{"auth":{"client_token":"t-approle","lease_duration":3600}}`))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
		tok := r.Header.Get("X-Vault-Token")
		if (tok != "t-static" && tok != "t-approle") || (tok == "t-approle" && f.revoked.Load()) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.reads.Add(1)
		switch strings.TrimPrefix(r.URL.Path, "/v1/kv/data/") {
		case "mro/client":
			_, _ = w.Write([]byte(`{"data":{"data":{"clientId":"c","clientSecret":"s","tokenUrl":"https://idp/token"}}}`))
		case "mro/api":
			_, _ = w.Write([]byte(`{"data":{"data":{"value":"k-123"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestVaultStaticToken(t *testing.T) {
	fv := &fakeVault{}
	srv := httptest.NewServer(fv)
	defer srv.Close()
	v := &VaultSecrets{Addr: srv.URL, Mount: "kv", Token: "t-static"}
	ctx := context.Background()

	if s, err := v.Secret(ctx, "vault://mro/api"); err != nil || s != "k-123" {
		t.Errorf("single value = %q, %v", s, err)
	}
	if s, err := v.Secret(ctx, "vault://mro/client#clientId"); err != nil || s != "c" {
		t.Errorf("field = %q, %v", s, err)
	}
	whole, err := v.Secret(ctx, "vault://mro/client")
	var m map[string]string
	if err != nil || json.Unmarshal([]byte(whole), &m) != nil || m["tokenUrl"] != "https://idp/token" {
		t.Errorf("whole secret = %q, %v", whole, err)
	}
	if fv.reads.Load() != 2 {
		t.Errorf("vault reads = %d, want 2 (one per path, then cached)", fv.reads.Load())
	}
	for _, ref := range []string{"vault://mro/client#nope", "vault://missing", "vault://", "vault://a/../b", "env://X"} {
		if _, err := v.Secret(ctx, ref); err == nil {
			t.Errorf("%s must fail", ref)
		}
	}
}

func TestVaultAppRoleAndCacheExpiry(t *testing.T) {
	fv := &fakeVault{}
	srv := httptest.NewServer(fv)
	defer srv.Close()
	clock := time.Unix(0, 0)
	v := &VaultSecrets{Addr: srv.URL, Mount: "kv", RoleID: "role", SecretID: "sid", CacheTTL: time.Minute,
		now: func() time.Time { return clock }}
	ctx := context.Background()
	if s, err := v.Secret(ctx, "vault://mro/api"); err != nil || s != "k-123" {
		t.Fatalf("approle read = %q, %v", s, err)
	}
	clock = clock.Add(2 * time.Minute) // cache expired, token still valid
	if _, err := v.Secret(ctx, "vault://mro/api"); err != nil {
		t.Fatal(err)
	}
	if fv.logins.Load() != 1 || fv.reads.Load() != 2 {
		t.Errorf("logins = %d, reads = %d; want 1, 2", fv.logins.Load(), fv.reads.Load())
	}
	fv.revoked.Store(true) // token revoked server-side: one re-login, then success
	clock = clock.Add(2 * time.Minute)
	if _, err := v.Secret(ctx, "vault://mro/api"); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	if fv.logins.Load() != 2 {
		t.Errorf("logins = %d, want 2", fv.logins.Load())
	}
}

func TestSecretsRouting(t *testing.T) {
	t.Setenv("ONTO_TEST_SECRET", "from-env")
	s := Secrets{}
	ctx := context.Background()
	if v, err := s.Secret(ctx, "env://ONTO_TEST_SECRET"); err != nil || v != "from-env" {
		t.Errorf("env = %q, %v", v, err)
	}
	if _, err := s.Secret(ctx, "vault://x"); err == nil || !strings.Contains(err.Error(), "VAULT_ADDR") {
		t.Errorf("vault without config: %v", err)
	}
	if _, err := s.Secret(ctx, "file:///etc/passwd"); err == nil {
		t.Error("unknown schemes must fail")
	}
}

func TestVaultFromEnv(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	if v, err := VaultFromEnv(); v != nil || err != nil {
		t.Errorf("unset = %v, %v", v, err)
	}
	t.Setenv("VAULT_ADDR", "http://vault:8200")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "")
	if _, err := VaultFromEnv(); err == nil {
		t.Error("an address without credentials must fail")
	}
	t.Setenv("VAULT_TOKEN", "t")
	if v, err := VaultFromEnv(); err != nil || v.Token != "t" {
		t.Errorf("token = %+v, %v", v, err)
	}
}
