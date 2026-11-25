package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acme/ontology/internal/platform/apperr"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(make([]byte, n-len(b)), b...)
}

type idp struct {
	srv  *httptest.Server
	mu   sync.Mutex
	keys []map[string]string
	hits int
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/jwks"})
		case "/jwks":
			p.mu.Lock()
			p.hits++
			keys := append([]map[string]string(nil), p.keys...)
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func (p *idp) addRSA(kid string, k *rsa.PublicKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, map[string]string{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())})
}

func (p *idp) addEC(kid string, k *ecdsa.PublicKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, map[string]string{"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(pad(k.X.Bytes(), 32)), "y": b64(pad(k.Y.Bytes(), 32))})
}

func sign(t *testing.T, alg, kid string, key crypto.Signer, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	input := b64(hdr) + "." + b64(body)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		s, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = s
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = append(pad(r.Bytes(), 32), pad(s.Bytes(), 32)...)
	}
	return input + "." + b64(sig)
}

type fixture struct {
	idp *idp
	v   *Verifier
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func setup(t *testing.T) *fixture {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := newIDP(t)
	p.addRSA("r1", &rk.PublicKey)
	p.addEC("e1", &ek.PublicKey)
	v, err := NewOIDCVerifier(context.Background(), p.srv.URL, []string{"ontology"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	v.Keys.(*JWKS).MinRefresh = time.Nanosecond
	return &fixture{idp: p, v: v, rsa: rk, ec: ek}
}

// claims returns valid claims; a nil value in extra deletes that claim.
func (f *fixture) claims(extra map[string]any) map[string]any {
	c := map[string]any{"iss": f.idp.srv.URL, "aud": "ontology", "sub": "alice", "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

func TestVerifyValidTokens(t *testing.T) {
	f := setup(t)
	for _, tok := range []string{
		sign(t, "RS256", "r1", f.rsa, f.claims(nil)),
		sign(t, "ES256", "e1", f.ec, f.claims(map[string]any{"aud": []string{"other", "ontology"}})),
	} {
		c, err := f.v.Verify(context.Background(), tok)
		if err != nil || c.String("sub") != "alice" {
			t.Fatalf("claims=%v err=%v", c, err)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	f := setup(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	valid := strings.Split(sign(t, "RS256", "r1", f.rsa, f.claims(nil)), ".")
	forged, _ := json.Marshal(f.claims(map[string]any{"sub": "mallory"}))
	none := b64([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + valid[1] + "."
	hs := b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + valid[1] + "." + valid[2]
	cases := map[string]string{
		"expired":        sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})),
		"not yet valid":  sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})),
		"missing exp":    sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"exp": nil})),
		"wrong audience": sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"aud": "billing"})),
		"wrong issuer":   sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"iss": "https://evil.example"})),
		"wrong key":      sign(t, "RS256", "r1", other, f.claims(nil)),
		"unknown kid":    sign(t, "RS256", "nope", f.rsa, f.claims(nil)),
		"tampered":       valid[0] + "." + b64(forged) + "." + valid[2],
		"alg none":       none,
		"alg HS256":      hs,
		"garbage":        "abc",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestUnknownKidRefreshesJWKS(t *testing.T) { // key rotation
	f := setup(t)
	if _, err := f.v.Verify(context.Background(), sign(t, "RS256", "r1", f.rsa, f.claims(nil))); err != nil {
		t.Fatal(err)
	}
	before := f.idp.count()
	k2, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.idp.addEC("e2", &k2.PublicKey)
	if _, err := f.v.Verify(context.Background(), sign(t, "ES256", "e2", k2, f.claims(nil))); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
	if got := f.idp.count(); got != before+1 {
		t.Errorf("JWKS fetches = %d, want %d", got, before+1)
	}
}

func TestMappingPrincipal(t *testing.T) {
	p, err := Mapping{}.Principal(Claims{"sub": "alice", "groups": []any{"g1", "g2"}})
	if err != nil || p.ID != "alice" || p.Kind != KindUser || len(p.Groups) != 2 {
		t.Fatalf("user = %+v, %v", p, err)
	}
	p, err = Mapping{}.Principal(Claims{"sub": "alice", "act": map[string]any{"sub": "agent-7"}})
	if err != nil || p.ID != "agent-7" || p.Kind != KindAgent || p.OnBehalfOf != "alice" {
		t.Fatalf("agent = %+v, %v", p, err)
	}
	p, err = Mapping{GroupsClaim: "roles"}.Principal(Claims{"sub": "svc", "roles": "admin", "actor_kind": "service"})
	if err != nil || p.Kind != KindService || len(p.Groups) != 1 || p.Groups[0] != "admin" {
		t.Fatalf("service = %+v, %v", p, err)
	}
	for _, c := range []Claims{
		{"act": map[string]any{"sub": "agent"}},
		{"sub": "x", "act": map[string]any{}},
		{"sub": "x", "actor_kind": "ROBOT"},
	} {
		if _, err := (Mapping{}).Principal(c); err == nil {
			t.Errorf("claims %v: expected an error", c)
		}
	}
}

func TestBearerAuthenticator(t *testing.T) {
	f := setup(t)
	a := &BearerAuthenticator{Verifier: f.v}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	var ae *apperr.Error
	if _, err := a.Authenticate(r); !errors.As(err, &ae) || ae.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("missing token: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"act": map[string]any{"sub": "agent-1"}})))
	p, err := a.Authenticate(r)
	if err != nil || p.ID != "agent-1" || p.OnBehalfOf != "alice" {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	r.Header.Set("Authorization", "Bearer "+sign(t, "RS256", "r1", f.rsa, f.claims(map[string]any{"aud": "billing"})))
	if _, err := a.Authenticate(r); !errors.As(err, &ae) || ae.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("wrong audience: %v", err)
	}
}

func TestDiscoveryChecks(t *testing.T) {
	p := newIDP(t)
	if _, err := NewOIDCVerifier(context.Background(), p.srv.URL+"/", []string{"x"}, Options{}); err == nil {
		t.Error("issuer mismatch must be rejected")
	}
	if _, err := NewOIDCVerifier(context.Background(), "http://idp.example.com", []string{"x"}, Options{}); err == nil {
		t.Error("plain http to a non-loopback host must be rejected")
	}
	if _, err := NewOIDCVerifier(context.Background(), p.srv.URL, nil, Options{}); err == nil {
		t.Error("an audience is required")
	}
}

func TestHeaderAuthenticator(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := (HeaderAuthenticator{}).Authenticate(r); err == nil {
		t.Fatal("missing X-Actor must fail")
	}
	r.Header.Set("X-Actor", "bob")
	r.Header.Set("X-Actor-Groups", "a, b")
	p, err := HeaderAuthenticator{}.Authenticate(r)
	if err != nil || p.Kind != KindUser || len(p.Groups) != 2 || !HasAnyGroup(p, []string{"b"}) {
		t.Fatalf("principal = %+v, %v", p, err)
	}
}
