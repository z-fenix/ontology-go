package objstore

import (
	"os"
	"strings"
	"testing"

	"github.com/acme/ontology/internal/spec"
)

func loadExample(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if diags := append(ld, cd...); diags.HasErrors(false) {
		t.Fatalf("example ontology has errors: %v", diags.Codes())
	}
	return ont
}

func findStmt(stmts []string, substr string) string {
	for _, s := range stmts {
		if strings.Contains(s, substr) {
			return s
		}
	}
	return ""
}

func TestGenerateDDL(t *testing.T) {
	stmts, err := GenerateDDL(loadExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if stmts[0] != `CREATE SCHEMA IF NOT EXISTS "obj"` {
		t.Errorf("first statement = %q", stmts[0])
	}
	wo := findStmt(stmts, `CREATE TABLE IF NOT EXISTS "obj"."work_order"`)
	if wo == "" {
		t.Fatal("work_order table missing")
	}
	for _, want := range []string{
		`"_pk" TEXT PRIMARY KEY`,
		`"work_order_id" TEXT`,
		`"priority" INTEGER`,
		`"due_at" TIMESTAMPTZ`,
		`"estimated_cost" NUMERIC(14,2)`,
	} {
		if !strings.Contains(wo, want) {
			t.Errorf("work_order DDL lacks %s:\n%s", want, wo)
		}
	}
	if strings.Contains(wo, "description_embedding") {
		t.Error("vector property must not be stored in PostgreSQL")
	}
	tech := findStmt(stmts, `"obj"."technician" (`)
	if !strings.Contains(tech, `"certifications" JSONB`) {
		t.Errorf("array property should be JSONB:\n%s", tech)
	}
	if findStmt(stmts, `CREATE TABLE IF NOT EXISTS "obj"."link_aircraft_parts"`) == "" {
		t.Error("M:N link table missing")
	}
	for _, idx := range []string{`"ix_work_order_tail_number"`, `"ix_work_order_technician_id"`, `"ix_link_aircraft_parts_to_pk"`} {
		if findStmt(stmts, idx) == "" {
			t.Errorf("index %s missing", idx)
		}
	}
	again, _ := GenerateDDL(loadExample(t))
	if strings.Join(stmts, ";") != strings.Join(again, ";") {
		t.Error("DDL is not deterministic")
	}
}

func TestPlanDDLAddColumn(t *testing.T) {
	old, cur := loadExample(t), loadExample(t)
	ac := cur.ObjectTypes["Aircraft"]
	ac.Properties["registeredAt"] = &spec.Property{APIName: "registeredAt", Type: spec.TypeRef{Base: spec.TTimestamp}}
	ac.PropertyOrder = append(ac.PropertyOrder, "registeredAt")
	stmts, err := PlanDDL(old, cur)
	if err != nil {
		t.Fatal(err)
	}
	want := `ALTER TABLE "obj"."aircraft" ADD COLUMN IF NOT EXISTS "registered_at" TIMESTAMPTZ`
	if findStmt(stmts, want) == "" {
		t.Errorf("missing %q in %v", want, stmts)
	}
	if findStmt(stmts, "CREATE TABLE IF NOT EXISTS \"obj\".\"aircraft\"") != "" {
		t.Error("existing table must not be re-created")
	}
}

func TestPlanDDLTypeChangeRejected(t *testing.T) {
	old, cur := loadExample(t), loadExample(t)
	cur.ObjectTypes["Aircraft"].Properties["flightHours"].Type = spec.TypeRef{Base: spec.TString}
	if _, err := PlanDDL(old, cur); err == nil || !strings.Contains(err.Error(), "castProperty") {
		t.Fatalf("expected castProperty error, got %v", err)
	}
}

func TestIndexNameTruncation(t *testing.T) {
	n := indexName(strings.Repeat("t", 50), strings.Repeat("c", 30))
	if len(n) > maxIdentLen {
		t.Fatalf("index name too long: %d", len(n))
	}
	if n != indexName(strings.Repeat("t", 50), strings.Repeat("c", 30)) {
		t.Fatal("index name not deterministic")
	}
}
