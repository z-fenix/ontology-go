package main

import (
    "encoding/json"
    "errors"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

type captured struct {
    path, actor, auth string
    body              publishRequest
}

func fakeOMS(t *testing.T, status int, resp any) (*httptest.Server, *captured) {
    t.Helper()
    c := &captured{}
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        c.path, c.actor, c.auth = r.URL.Path, r.Header.Get("X-Actor"), r.Header.Get("Authorization")
        b, _ := io.ReadAll(r.Body)
        if err := json.Unmarshal(b, &c.body); err != nil {
            t.Errorf("bad request body: %v", err)
        }
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(status)
        _ = json.NewEncoder(w).Encode(resp)
    }))
    t.Cleanup(srv.Close)
    return srv, c
}

func TestPublishSendsSourcesAndPrintsVersion(t *testing.T) {
    srv, c := fakeOMS(t, http.StatusCreated, map[string]any{
        "version": 3, "status": "ACTIVE",
        "changeset": map[string]any{"changes": []map[string]any{{"kind": "add_property", "path": "objectTypes.WorkOrder.properties.note"}}},
    })
    t.Setenv("ONTO_TOKEN", "tok")
    out, err := run("publish", "../../examples/ontology", "--oms", srv.URL, "--git-commit", "abc123", "--actor", "me@acme")
    if err != nil {
        t.Fatalf("publish failed: %v\n%s", err, out)
    }
    if c.path != "/api/v1/ontologies/mro/versions" || c.actor != "me@acme" || c.auth != "Bearer tok" {
        t.Errorf("request path=%q actor=%q auth=%q", c.path, c.actor, c.auth)
    }
    if c.body.GitCommit != "abc123" || len(c.body.Files) < 8 {
        t.Errorf("body commit=%q files=%d", c.body.GitCommit, len(c.body.Files))
    }
    for _, f := range c.body.Files {
        if strings.Contains(f.Path, "\\") || strings.HasPrefix(f.Path, "/") || strings.HasPrefix(f.Path, "../") {
            t.Errorf("file path must be relative and slash-separated: %q", f.Path)
        }
    }
    if !strings.Contains(out, "published mro version 3 (ACTIVE)") || !strings.Contains(out, "add_property") {
        t.Errorf("output: %s", out)
    }
}

func TestPublishRejectedPrintsMissingMigrations(t *testing.T) {
    srv, _ := fakeOMS(t, http.StatusConflict, map[string]any{
        "errorCode": "PUB002", "message": "1 breaking change(s) require a migration",
        "details": map[string]any{"missingMigrations": []map[string]any{
            {"kind": "drop_property", "path": "objectTypes.WorkOrder.properties.status", "detail": "property removed"}}},
    })
    out, err := run("publish", "../../examples/ontology", "--oms", srv.URL, "--git-commit", "abc")
    if !errors.Is(err, errFailed) {
        t.Fatalf("expected errFailed, got %v", err)
    }
    if !strings.Contains(out, "PUB002") || !strings.Contains(out, "drop_property") {
        t.Errorf("output: %s", out)
    }
}

func TestPublishDryRunDoesNotNeedCommit(t *testing.T) {
    srv, c := fakeOMS(t, http.StatusCreated, map[string]any{"version": 2, "status": "DRY_RUN"})
    out, err := run("publish", "../../examples/ontology", "--oms", srv.URL, "--dry-run")
    if err != nil || !c.body.DryRun || !strings.Contains(out, "dry run") {
        t.Fatalf("err=%v dryRun=%v out=%s", err, c.body.DryRun, out)
    }
}

func TestPublishRequiresOMSAndCommit(t *testing.T) {
    t.Setenv("ONTO_OMS_URL", "")
    t.Setenv("GITHUB_SHA", "")
    if _, err := run("publish", "../../examples/ontology"); err == nil || !strings.Contains(err.Error(), "--oms") {
        t.Errorf("missing --oms: %v", err)
    }
    if _, err := run("publish", "../../examples/ontology", "--oms", "http://x"); err == nil || !strings.Contains(err.Error(), "--git-commit") {
        t.Errorf("missing --git-commit: %v", err)
    }
}

func TestPublishLocalValidationBlocksRequest(t *testing.T) {
    called := false
    srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
    defer srv.Close()
    _, err := run("publish", "does-not-exist", "--oms", srv.URL, "--git-commit", "abc")
    if !errors.Is(err, errFailed) || called {
        t.Fatalf("err=%v called=%v", err, called)
    }
}
