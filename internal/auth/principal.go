package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/acme/ontology/internal/platform/apperr"
)

// Principal kinds.
const (
	KindUser    = "USER"
	KindAgent   = "AGENT"
	KindService = "SERVICE"
)

// Principal is an authenticated caller.
type Principal struct {
	ID         string
	Kind       string
	OnBehalfOf string // for agents: the user the agent acts for
	Groups     []string
}

// Authenticator identifies the caller of an HTTP request.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// Mapping turns token claims into a principal.
type Mapping struct {
	GroupsClaim string // default "groups"
	KindClaim   string // default "actor_kind"; values USER or SERVICE
}

// Principal maps claims. A delegation token (RFC 8693) carries the user in "sub" and the acting
// agent in "act.sub"; it maps to an AGENT principal acting on behalf of that user.
func (m Mapping) Principal(c Claims) (Principal, error) {
	groupsClaim, kindClaim := m.GroupsClaim, m.KindClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	if kindClaim == "" {
		kindClaim = "actor_kind"
	}
	sub := c.String("sub")
	if sub == "" {
		return Principal{}, invalid("the sub claim is required")
	}
	p := Principal{ID: sub, Kind: KindUser, Groups: c.Strings(groupsClaim)}
	if act, ok := c["act"].(map[string]any); ok {
		agent, _ := act["sub"].(string)
		if agent == "" {
			return Principal{}, invalid("the act claim must contain sub")
		}
		p.ID, p.Kind, p.OnBehalfOf = agent, KindAgent, sub
		return p, nil
	}
	switch k := strings.ToUpper(c.String(kindClaim)); k {
	case "", KindUser:
	case KindService:
		p.Kind = KindService
	case KindAgent:
		p.Kind = KindAgent // no act claim: the agent acts for nobody, so the action engine denies it
	default:
		return Principal{}, invalid("unknown %s %q", kindClaim, k)
	}
	return p, nil
}

func unauthenticated(format string, args ...any) error {
	return apperr.New(http.StatusUnauthorized, "UNAUTHENTICATED", format, args...)
}

// BearerAuthenticator verifies "Authorization: Bearer <JWT>".
type BearerAuthenticator struct {
	Verifier *Verifier
	Mapping  Mapping
}

// Authenticate implements Authenticator.
func (b *BearerAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Principal{}, unauthenticated("a bearer token is required")
	}
	claims, err := b.Verifier.Verify(r.Context(), token)
	if err != nil {
		return Principal{}, unauthenticated("%v", err)
	}
	p, err := b.Mapping.Principal(claims)
	if err != nil {
		return Principal{}, unauthenticated("%v", err)
	}
	return p, nil
}

// HeaderAuthenticator trusts X-Actor, X-Actor-Kind, X-On-Behalf-Of and X-Actor-Groups.
// Local development only: anyone can claim any identity.
type HeaderAuthenticator struct{}

// Authenticate implements Authenticator.
func (HeaderAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	p := Principal{ID: r.Header.Get("X-Actor"), Kind: strings.ToUpper(r.Header.Get("X-Actor-Kind")),
		OnBehalfOf: r.Header.Get("X-On-Behalf-Of"), Groups: SplitList(r.Header.Get("X-Actor-Groups"))}
	if p.ID == "" {
		return Principal{}, unauthenticated("missing X-Actor header")
	}
	if p.Kind == "" {
		p.Kind = KindUser
	}
	if p.Kind != KindUser && p.Kind != KindAgent && p.Kind != KindService {
		return Principal{}, apperr.BadRequest("INVALID_ACTOR", "X-Actor-Kind must be USER, AGENT or SERVICE")
	}
	return p, nil
}

// SplitList splits a comma-separated list, dropping empty items.
func SplitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// HasAnyGroup reports whether p belongs to at least one of groups.
func HasAnyGroup(p Principal, groups []string) bool {
	return intersects(p.Groups, groups)
}

// FromEnv builds the authenticator from configuration:
//
//	ONTO_AUTH_DEV_HEADERS=true   trust X-Actor headers (development only)
//	ONTO_OIDC_ISSUER             issuer URL (required otherwise)
//	ONTO_OIDC_AUDIENCE           comma-separated accepted audiences (required otherwise)
//	ONTO_OIDC_GROUPS_CLAIM       default "groups"
//	ONTO_OIDC_KIND_CLAIM         default "actor_kind"
//	ONTO_OIDC_ALLOW_HTTP=true    allow a plain-http issuer on a non-loopback host (development only)
//
// The boolean result reports development mode.
func FromEnv(ctx context.Context) (Authenticator, bool, error) {
	if os.Getenv("ONTO_AUTH_DEV_HEADERS") == "true" {
		slog.Warn("authentication trusts unverified X-Actor headers (ONTO_AUTH_DEV_HEADERS=true); development only")
		return HeaderAuthenticator{}, true, nil
	}
	issuer := os.Getenv("ONTO_OIDC_ISSUER")
	audiences := SplitList(os.Getenv("ONTO_OIDC_AUDIENCE"))
	if issuer == "" || len(audiences) == 0 {
		return nil, false, fmt.Errorf("set ONTO_OIDC_ISSUER and ONTO_OIDC_AUDIENCE (or ONTO_AUTH_DEV_HEADERS=true for local development)")
	}
	v, err := NewOIDCVerifier(ctx, issuer, audiences, Options{AllowHTTP: os.Getenv("ONTO_OIDC_ALLOW_HTTP") == "true"})
	if err != nil {
		return nil, false, err
	}
	return &BearerAuthenticator{Verifier: v, Mapping: Mapping{
		GroupsClaim: os.Getenv("ONTO_OIDC_GROUPS_CLAIM"), KindClaim: os.Getenv("ONTO_OIDC_KIND_CLAIM"),
	}}, false, nil
}
