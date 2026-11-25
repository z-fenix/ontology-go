// Package auth verifies OIDC bearer tokens and maps their claims to principals (Spec §8.1).
//
// It uses only the standard library: RS256/RS384/RS512 and ES256/ES384 signatures, with keys
// taken from the issuer's JWKS (located through OIDC discovery), cached, and refreshed when a
// token names an unknown key ID. "none" and HMAC algorithms are always rejected.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256" // registers SHA-256 for crypto.Hash.New
	_ "crypto/sha512" // registers SHA-384 and SHA-512
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrInvalidToken wraps every verification failure.
var ErrInvalidToken = errors.New("invalid token")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(format, args...))
}

// Claims is a decoded JWT payload.
type Claims map[string]any

// String returns a string claim, or "" when it is absent or not a string.
func (c Claims) String(name string) string {
	s, _ := c[name].(string)
	return s
}

// Strings returns a claim that may be a single string or an array of strings.
func (c Claims) Strings(name string) []string {
	switch v := c[name].(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c Claims) unix(name string) (time.Time, bool) {
	f, ok := c[name].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// KeySource resolves a verification key by key ID and algorithm.
type KeySource interface {
	Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error)
}

// Verifier checks a token's signature, issuer, audience and validity window.
type Verifier struct {
	Issuer    string
	Audiences []string
	Keys      KeySource
	Leeway    time.Duration    // allowed clock skew (default 60s)
	Now       func() time.Time // overridable in tests
}

var algHash = map[string]crypto.Hash{
	"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512,
	"ES256": crypto.SHA256, "ES384": crypto.SHA384,
}

// Verify validates a compact JWS token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, invalid("a token must have three segments")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, invalid("header: %v", err)
	}
	hash, ok := algHash[hdr.Alg]
	if !ok {
		return nil, invalid("algorithm %q is not allowed", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil {
		return nil, invalid("signature encoding: %v", err)
	}
	key, err := v.Keys.Key(ctx, hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(hdr.Alg, hash, key, parts[0]+"."+parts[1], sig); err != nil {
		return nil, err
	}
	var claims Claims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, invalid("payload: %v", err)
	}
	if err := v.checkClaims(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func (v *Verifier) checkClaims(c Claims) error {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	leeway := v.Leeway
	if leeway == 0 {
		leeway = time.Minute
	}
	if iss := c.String("iss"); iss != v.Issuer {
		return invalid("issuer %q is not trusted", iss)
	}
	if !intersects(c.Strings("aud"), v.Audiences) {
		return invalid("audience %v does not include any of %v", c.Strings("aud"), v.Audiences)
	}
	exp, ok := c.unix("exp")
	if !ok {
		return invalid("the exp claim is required")
	}
	if now.After(exp.Add(leeway)) {
		return invalid("token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if nbf, ok := c.unix("nbf"); ok && now.Add(leeway).Before(nbf) {
		return invalid("token is not valid before %s", nbf.UTC().Format(time.RFC3339))
	}
	return nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func verifySignature(alg string, hash crypto.Hash, key crypto.PublicKey, signingInput string, sig []byte) error {
	h := hash.New()
	h.Write([]byte(signingInput))
	digest := h.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if !strings.HasPrefix(alg, "RS") {
			return invalid("an RSA key cannot verify %s", alg)
		}
		if err := rsa.VerifyPKCS1v15(k, hash, digest, sig); err != nil {
			return invalid("signature verification failed")
		}
		return nil
	case *ecdsa.PublicKey:
		want := map[string]string{"ES256": "P-256", "ES384": "P-384"}[alg]
		if want == "" || k.Curve.Params().Name != want {
			return invalid("the key curve does not match %s", alg)
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return invalid("signature verification failed")
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return invalid("signature verification failed")
		}
		return nil
	}
	return invalid("unsupported key type %T", key)
}

// ---------- JWKS ----------

// JWKS fetches and caches an issuer's signing keys.
type JWKS struct {
	URL        string
	HTTP       *http.Client
	AllowHTTP  bool          // permit plain http to non-loopback hosts (development only)
	TTL        time.Duration // scheduled refresh interval (default 10m)
	MinRefresh time.Duration // minimum gap between fetches (default 30s)

	mu        sync.Mutex
	keys      []jwk
	fetched   time.Time
	attempted time.Time
}

type jwk struct {
	kid string
	alg string
	pub crypto.PublicKey
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Key returns the key for kid, refetching the JWKS when the key is unknown (key rotation).
func (j *JWKS) Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	minGap := orDefault(j.MinRefresh, 30*time.Second)
	due := j.keys == nil || time.Since(j.fetched) > orDefault(j.TTL, 10*time.Minute)
	refreshed := false
	var fetchErr error
	if due && time.Since(j.attempted) >= minGap {
		fetchErr = j.refresh(ctx)
		refreshed = true
	}
	if j.keys == nil {
		return nil, fmt.Errorf("%w: signing keys are unavailable: %v", ErrInvalidToken, fetchErr)
	}
	if k := j.lookup(kid, alg); k != nil {
		return k, nil
	}
	if !refreshed && time.Since(j.attempted) >= minGap {
		if err := j.refresh(ctx); err == nil {
			if k := j.lookup(kid, alg); k != nil {
				return k, nil
			}
		}
	}
	return nil, invalid("no signing key matches kid %q", kid)
}

func keyFits(pub crypto.PublicKey, alg string) bool {
	switch pub.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS")
	case *ecdsa.PublicKey:
		return strings.HasPrefix(alg, "ES")
	}
	return false
}

func (j *JWKS) lookup(kid, alg string) crypto.PublicKey {
	var match []jwk
	for _, k := range j.keys {
		if (k.alg != "" && k.alg != alg) || !keyFits(k.pub, alg) || (kid != "" && k.kid != kid) {
			continue
		}
		match = append(match, k)
	}
	// Without a kid the choice is only safe when exactly one key fits.
	if len(match) == 1 || (kid != "" && len(match) > 0) {
		return match[0].pub
	}
	return nil
}

func (j *JWKS) refresh(ctx context.Context) error {
	j.attempted = time.Now()
	body, err := getJSON(ctx, j.HTTP, j.URL, j.AllowHTTP)
	if err != nil {
		return err
	}
	var doc struct {
		Keys []rawJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}
	var keys []jwk
	for _, r := range doc.Keys {
		if r.Use != "" && r.Use != "sig" {
			continue
		}
		pub, err := r.publicKey()
		if err != nil {
			continue // unsupported or malformed keys are skipped, not fatal
		}
		keys = append(keys, jwk{kid: r.Kid, alg: r.Alg, pub: pub})
	}
	if len(keys) == 0 {
		return fmt.Errorf("JWKS %s contains no usable signing keys", j.URL)
	}
	j.keys, j.fetched = keys, time.Now()
	return nil
}

type rawJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil, errors.New("invalid base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}

func (r rawJWK) publicKey() (crypto.PublicKey, error) {
	switch r.Kty {
	case "RSA":
		n, err1 := b64int(r.N)
		e, err2 := b64int(r.E)
		if err1 != nil || err2 != nil || n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, errors.New("invalid RSA key")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch r.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return nil, fmt.Errorf("unsupported curve %q", r.Crv)
		}
		x, err1 := b64int(r.X)
		y, err2 := b64int(r.Y)
		if err1 != nil || err2 != nil || !curve.IsOnCurve(x, y) {
			return nil, errors.New("invalid EC key")
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	}
	return nil, fmt.Errorf("unsupported key type %q", r.Kty)
}

// ---------- discovery ----------

// Options configures discovery and key fetching.
type Options struct {
	HTTP      *http.Client
	AllowHTTP bool // permit plain http to non-loopback hosts (development only)
}

func checkURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if allowHTTP || host == "localhost" || net.ParseIP(host).IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s must use https", raw)
}

func getJSON(ctx context.Context, client *http.Client, rawURL string, allowHTTP bool) ([]byte, error) {
	if err := checkURL(rawURL, allowHTTP); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// Discover reads the issuer's OIDC configuration and returns its jwks_uri.
func Discover(ctx context.Context, issuer string, opts Options) (string, error) {
	body, err := getJSON(ctx, opts.HTTP, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", opts.AllowHTTP)
	if err != nil {
		return "", fmt.Errorf("OIDC discovery: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("OIDC discovery: %w", err)
	}
	if doc.Issuer != issuer {
		return "", fmt.Errorf("OIDC discovery: issuer %q does not match the configured %q", doc.Issuer, issuer)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("OIDC discovery: jwks_uri is missing")
	}
	return doc.JWKSURI, nil
}

// NewOIDCVerifier discovers the issuer's JWKS and returns a verifier for the given audiences.
func NewOIDCVerifier(ctx context.Context, issuer string, audiences []string, opts Options) (*Verifier, error) {
	if issuer == "" || len(audiences) == 0 {
		return nil, errors.New("an issuer and at least one audience are required")
	}
	uri, err := Discover(ctx, issuer, opts)
	if err != nil {
		return nil, err
	}
	return &Verifier{Issuer: issuer, Audiences: audiences,
		Keys: &JWKS{URL: uri, HTTP: opts.HTTP, AllowHTTP: opts.AllowHTTP}}, nil
}
