package action

import (
	"net/http"

	"github.com/acme/ontology/internal/auth"
)

// IdentityFrom adapts an authenticator (OIDC bearer tokens, or dev headers) to the handler.
func IdentityFrom(a auth.Authenticator) IdentityFunc {
	return func(r *http.Request) (Actor, error) {
		p, err := a.Authenticate(r)
		if err != nil {
			return Actor{}, err
		}
		return Actor{ID: p.ID, Kind: ActorKind(p.Kind), OnBehalfOf: p.OnBehalfOf, Groups: p.Groups}, nil
	}
}
