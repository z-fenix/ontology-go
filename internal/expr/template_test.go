package expr

import "testing"

func TestParseTemplate(t *testing.T) {
	whole := ParseTemplate("{{ technician.technicianId }}")
	if !whole.IsWholeExpr() || len(whole.Exprs()) != 1 {
		t.Errorf("whole-expr parse failed: %+v", whole)
	}
	mixed := ParseTemplate("hi {{user.id}} and {{ note }}")
	if mixed.IsWholeExpr() {
		t.Error("mixed template should not be whole-expr")
	}
	if len(mixed.Exprs()) != 2 {
		t.Errorf("exprs = %v", mixed.Exprs())
	}
	none := ParseTemplate("IN_PROGRESS")
	if none.IsWholeExpr() || len(none.Exprs()) != 0 {
		t.Errorf("literal parse failed: %+v", none)
	}
}

func TestRenderWholeExprKeepsType(t *testing.T) {
	env, _ := NewEnv(testAction())
	tmpl := ParseTemplate("{{ workOrder.priority }}")
	v, err := tmpl.Render(env, map[string]any{
		"workOrder": map[string]any{"priority": int64(3)}, "technician": map[string]any{}, "note": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(int64); !ok {
		t.Errorf("whole-expr render = %#v, want int64", v)
	}
}

func TestRenderMixedToString(t *testing.T) {
	env, _ := NewEnv(testAction())
	tmpl := ParseTemplate("note: {{ note }}")
	v, err := tmpl.Render(env, map[string]any{
		"workOrder": map[string]any{}, "technician": map[string]any{}, "note": "urgent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != "note: urgent" {
		t.Errorf("mixed render = %#v", v)
	}
}
