package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acme/ontology/internal/spec"
)

func server(status int, capture func(r *http.Request, body map[string]any)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if capture != nil {
			capture(r, body)
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "https://evil.example/")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func TestDoSuccessSendsAuthAndIdempotencyKey(t *testing.T) {
	var auth, idem string
	var body map[string]any
	srv := server(http.StatusOK, func(r *http.Request, b map[string]any) {
		auth, idem, body = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), b
	})
	defer srv.Close()
	t.Setenv("MRO_TOKEN", "s3cret")
	x := NewExecutor(EnvSecrets{}, []string{"127.0.0.1"})
	resp, err := x.Do(context.Background(), Request{Name: "w", Method: "POST", URL: srv.URL + "/assign",
		Body: map[string]any{"technician": "T-1"}, Auth: spec.WebhookAuth{Type: "bearer", SecretRef: "env://MRO_TOKEN"}}, "rid:w:WRITEBACK")
	if err != nil || resp.Status != 200 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if auth != "Bearer s3cret" || idem != "rid:w:WRITEBACK" || body["technician"] != "T-1" {
		t.Errorf("auth=%q idem=%q body=%v", auth, idem, body)
	}
}

func TestDoClassification(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{{500, false}, {503, false}, {429, false}, {400, true}, {404, true}, {http.StatusFound, true}}
	for _, c := range cases {
		srv := server(c.status, nil)
		x := NewExecutor(nil, []string{"127.0.0.1"})
		_, err := x.Do(context.Background(), Request{URL: srv.URL}, "")
		srv.Close()
		if err == nil {
			t.Errorf("status %d: expected an error", c.status)
			continue
		}
		if IsPermanent(err) != c.permanent {
			t.Errorf("status %d: permanent=%v, want %v (%v)", c.status, IsPermanent(err), c.permanent, err)
		}
	}
}

func TestDoRejectsBeforeSending(t *testing.T) {
	called := false
	srv := server(200, func(*http.Request, map[string]any) { called = true })
	defer srv.Close()
	x := NewExecutor(EnvSecrets{}, []string{"mro.internal"})
	if _, err := x.Do(context.Background(), Request{URL: srv.URL}, ""); !IsPermanent(err) {
		t.Errorf("disallowed host: err=%v", err)
	}
	x.AllowedHosts = []string{"127.0.0.1"}
	if _, err := x.Do(context.Background(), Request{URL: srv.URL, Auth: spec.WebhookAuth{Type: "bearer", SecretRef: "env://NOT_SET_ANYWHERE"}}, ""); !IsPermanent(err) {
		t.Errorf("missing secret: err=%v", err)
	}
	if _, err := x.Do(context.Background(), Request{URL: "ftp://127.0.0.1/x"}, ""); !IsPermanent(err) {
		t.Errorf("bad scheme: err=%v", err)
	}
	if called {
		t.Error("no request may be sent when validation fails")
	}
}

func TestWildcardAllowlist(t *testing.T) {
	x := NewExecutor(nil, []string{"*.internal"})
	if !x.hostAllowed("mro.internal") || x.hostAllowed("internal.evil.com") {
		t.Error("wildcard matching is wrong")
	}
}
