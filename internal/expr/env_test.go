package expr

import (
	"testing"

	"github.com/acme/ontology/internal/spec"
)

func testAction() *spec.ActionType {
	return &spec.ActionType{
		APIName: "assignWorkOrder",
		Parameters: map[string]*spec.ActionParam{
			"workOrder":  {APIName: "workOrder", Type: spec.ParamType{Kind: spec.KindObjectRef, ObjectType: "WorkOrder"}},
			"technician": {APIName: "technician", Type: spec.ParamType{Kind: spec.KindObjectRef, ObjectType: "Technician"}},
			"note":       {APIName: "note", Type: spec.ParamType{Kind: spec.KindPrimitive, Base: spec.TString}},
		},
		ParamOrder: []string{"workOrder", "technician", "note"},
	}
}

func TestCompileBool(t *testing.T) {
	env, err := NewEnv(testAction())
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{
		`workOrder.status == "OPEN"`,
		`workOrder.priority <= 2`,
		`note == "" || size(note) < 100`,
		`workOrder.model in technician.certifications`,
	}
	for _, ex := range ok {
		if err := env.CompileBool(ex); err != nil {
			t.Errorf("CompileBool(%q) error: %v", ex, err)
		}
	}
	bad := []string{
		`workOrder.status =`,     // syntax error
		`workOrder.priority + 1`, // not bool
		`nope.field == 1`,        // unknown variable
	}
	for _, ex := range bad {
		if err := env.CompileBool(ex); err == nil {
			t.Errorf("CompileBool(%q) expected error", ex)
		}
	}
}

func TestEvalBool(t *testing.T) {
	env, _ := NewEnv(testAction())
	prg, err := env.Compile(`workOrder.status == "OPEN" && workOrder.priority <= 2`)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"workOrder":  map[string]any{"status": "OPEN", "priority": int64(1)},
		"technician": map[string]any{},
		"note":       "",
	}
	got, err := prg.EvalBool(vars)
	if err != nil || !got {
		t.Fatalf("EvalBool = %v, %v", got, err)
	}
	vars["workOrder"] = map[string]any{"status": "DONE", "priority": int64(1)}
	got, _ = prg.EvalBool(vars)
	if got {
		t.Error("expected false for DONE status")
	}
}

func TestReferencedParams(t *testing.T) {
	env, _ := NewEnv(testAction())
	refs := env.ReferencedParams(`workOrder.status == "OPEN"`)
	if len(refs) != 1 || refs[0] != "workOrder" {
		t.Errorf("ReferencedParams = %v", refs)
	}
}
