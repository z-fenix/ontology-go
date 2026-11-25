package main

import (
    "bytes"
    "encoding/json"
    "errors"
    "strings"
    "testing"
)

func run(args ...string) (string, error) {
    var out, errBuf bytes.Buffer
    root := newRootCmd(&out, &errBuf)
    root.SetArgs(args)
    err := root.Execute()
    return out.String() + errBuf.String(), err
}

func TestValidateExample(t *testing.T) {
    out, err := run("validate", "../../examples/ontology")
    if err != nil {
        t.Fatalf("validate failed: %v\n%s", err, out)
    }
    if !strings.Contains(out, "0 error(s), 0 warning(s)") {
        t.Errorf("unexpected output: %s", out)
    }
}

func TestValidateJSON(t *testing.T) {
    out, err := run("validate", "../../examples/ontology", "--format", "json")
    if err != nil {
        t.Fatalf("validate failed: %v", err)
    }
    var v struct {
        Diagnostics []any `json:"diagnostics"`
    }
    if err := json.Unmarshal([]byte(out), &v); err != nil {
        t.Fatalf("invalid json: %v\n%s", err, out)
    }
}

func TestValidateMissingDirFails(t *testing.T) {
    _, err := run("validate", "does-not-exist")
    if !errors.Is(err, errFailed) {
        t.Fatalf("expected errFailed, got %v", err)
    }
}

func TestExport(t *testing.T) {
    out, err := run("export", "../../examples/ontology")
    if err != nil {
        t.Fatalf("export failed: %v", err)
    }
    if !strings.Contains(out, `"checksum"`) || !strings.Contains(out, `"Aircraft"`) {
        t.Errorf("unexpected export output")
    }
}

func TestDDL(t *testing.T) {
    out, err := run("ddl", "../../examples/ontology")
    if err != nil {
        t.Fatalf("ddl failed: %v", err)
    }
    if !strings.Contains(out, `CREATE TABLE IF NOT EXISTS "obj"."work_order"`) {
        t.Errorf("unexpected ddl output:\n%s", out)
    }
}

func TestDiffNoBaselineSucceeds(t *testing.T) {
    out, err := run("diff", "../../examples/ontology")
    if err != nil {
        t.Fatalf("diff against nothing should succeed (all additive): %v\n%s", err, out)
    }
}
