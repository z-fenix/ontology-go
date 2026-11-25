package oms

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acme/ontology/internal/auth"
)

// Only rejection paths are exercised: they must short-circuit before the (nil) service is used.
func TestHandlerAuthentication(t *testing.T) {
	h := NewHandler(nil, nil, auth.HeaderAuthenticator{}, []string{"onto-owners"})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	do := func(method, path, actor string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{}`))
		if actor != "" {
			req.Header.Set("X-Actor", actor)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		method, path, actor string
		want                int
	}{
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodPost, "/api/v1/ontologies/mro/versions", "", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/ontologies/mro/versions", "bob", http.StatusForbidden},
		{http.MethodPost, "/api/v1/ontologies/mro/versions/1/activate", "bob", http.StatusForbidden},
		{http.MethodGet, "/api/v1/ontologies/mro/compiled", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.actor); got != c.want {
			t.Errorf("%s %s (actor %q) = %d, want %d", c.method, c.path, c.actor, got, c.want)
		}
	}
}
