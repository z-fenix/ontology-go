package spec

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

const baseOntology = `ontology:
  apiName: mro
  displayName: MRO
  connectors:
    mro_db: { kind: postgres, dsnRef: "vault://mro/db" }
`

const aircraftOK = `objectType:
  apiName: Aircraft
  primaryKey: tailNumber
  titleProperty: tailNumber
  properties:
    tailNumber: { type: string, required: true }
    baseAirport: string
  datasource:
    connector: mro_db
    table: public.aircraft
`

func compileMap(t *testing.T, files map[string]string) (*CompiledOntology, Diagnostics) {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	loaded, ldiags := LoadFS(fsys, "mem")
	ont, cdiags := Compile(loaded)
	all := append(ldiags, cdiags...)
	all.Sort()
	return ont, all
}

func hasCode(ds Diagnostics, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func replace(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("test fixture: substring not found: " + old)
	}
	return strings.Replace(s, old, new, 1)
}

func withOntology(extra map[string]string) map[string]string {
	m := map[string]string{"ontology.yaml": baseOntology}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestExampleOntologyIsValid(t *testing.T) {
	load := func() (*CompiledOntology, Diagnostics) {
		files, ld := LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
		ont, cd := Compile(files)
		return ont, append(ld, cd...)
	}
	ont, diags := load()
	if len(diags) != 0 {
		for _, d := range diags {
			t.Log(d.String())
		}
		t.Fatalf("expected no diagnostics, got %d", len(diags))
	}
	if got := len(ont.ObjectTypes); got != 4 {
		t.Errorf("object types = %d, want 4", got)
	}
	if got := len(ont.LinkTypes); got != 3 {
		t.Errorf("link types = %d, want 3", got)
	}
	if got := len(ont.ActionTypes); got != 2 {
		t.Errorf("action types = %d, want 2", got)
	}
	if got := len(ont.Webhooks); got != 1 {
		t.Errorf("webhooks = %d, want 1", got)
	}
	assign := ont.ActionTypes["assignWorkOrder"]
	if assign == nil || assign.Parameters["workOrder"].Type.Kind != KindObjectRef {
		t.Errorf("assignWorkOrder.workOrder param not objectRef")
	}
	if assign != nil && len(assign.SubmissionCriteria) != 2 {
		t.Errorf("assignWorkOrder criteria = %d, want 2", len(assign.SubmissionCriteria))
	}
	ac := ont.ObjectTypes["Aircraft"]
	if ac.TableName != "obj.aircraft" {
		t.Errorf("table = %q", ac.TableName)
	}
	if col := ac.Properties["flightHours"].Column; col != "total_fh" {
		t.Errorf("flightHours column = %q, want total_fh", col)
	}
	wo := ont.ObjectTypes["WorkOrder"]
	if col := wo.Properties["workOrderId"].Column; col != "work_order_id" {
		t.Errorf("workOrderId column = %q", col)
	}
	if s := wo.Properties["estimatedCost"].Sensitivity; s != SensConfidential {
		t.Errorf("estimatedCost sensitivity = %q", s)
	}
	if wo.Datasource.ConflictStrategy != MostRecentValue {
		t.Errorf("conflict strategy = %q", wo.Datasource.ConflictStrategy)
	}
	if ont.Checksum == "" {
		t.Fatal("checksum is empty")
	}
	ont2, _ := load()
	if ont.Checksum != ont2.Checksum {
		t.Error("checksum is not deterministic")
	}
}

func TestMinimalValid(t *testing.T) {
	_, diags := compileMap(t, withOntology(map[string]string{"aircraft.yaml": aircraftOK}))
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags.Codes())
	}
}

func TestRules(t *testing.T) {
	noDatasource := aircraftOK[:strings.Index(aircraftOK, "  datasource:")]
	cases := []struct {
		name  string
		files map[string]string
		code  string
	}{
		{"G001 missing ontology", map[string]string{"a.yaml": aircraftOK}, "G001"},
		{"G002 duplicate object type", withOntology(map[string]string{"a.yaml": aircraftOK, "b.yaml": aircraftOK}), "G002"},
		{"G003 bad type name", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "apiName: Aircraft", "apiName: aircraft")}), "G003"},
		{"G004 reserved property", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "baseAirport: string", "_pk: string")}), "G004"},
		{"G005 unknown field", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "required: true", "requird: true")}), "G005"},
		{"G007 unknown kind", withOntology(map[string]string{"x.yaml": "objectTypez: []\n"}), "G007"},
		{"VT001 non-scalar base", withOntology(map[string]string{"vt.yaml": "valueTypes:\n  - apiName: Emb\n    baseType: vector\n"}), "VT001"},
		{"VT002 regex on integer", withOntology(map[string]string{"vt.yaml": "valueTypes:\n  - apiName: Count\n    baseType: integer\n    constraints: { regex: \"^a$\" }\n"}), "VT002"},
		{"VT003 bad regex", withOntology(map[string]string{"vt.yaml": "valueTypes:\n  - apiName: Code\n    baseType: string\n    constraints: { regex: \"(\" }\n"}), "VT003"},
		{"OT001 pk not required", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "required: true", "required: false")}), "OT001"},
		{"OT002 title missing", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "titleProperty: tailNumber", "titleProperty: name")}), "OT002"},
		{"OT005 unknown connector", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "connector: mro_db", "connector: erp_db")}), "OT005"},
		{"OT006 missing timestampColumn", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "table: public.aircraft", "table: public.aircraft\n    conflictStrategy: MOST_RECENT_VALUE")}), "OT006"},
		{"OT007 column clash", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "table: public.aircraft", "table: public.aircraft\n    columnMapping: { baseAirport: tail_number }")}), "OT007"},
		{"OT008 bad derivedFrom", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "baseAirport: string", "emb: { type: vector(8), derivedFrom: missing }")}), "OT008"},
		{"OT009 no datasource (warning)", withOntology(map[string]string{"a.yaml": noDatasource}), "OT009"},
		{"OT010 authz unknown property", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "table: public.aircraft", "table: public.aircraft\n  authz:\n    fgaType: aircraft\n    tuples:\n      - { relation: base, target: \"base:{{homeBase}}\" }")}), "OT010"},
		{"OT011 bad type", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "baseAirport: string", "baseAirport: strng")}), "OT011"},
		{"OT015 columnMapping unknown property", withOntology(map[string]string{"a.yaml": replace(aircraftOK, "table: public.aircraft", "table: public.aircraft\n    columnMapping: { homeBase: base_iata }")}), "OT015"},
		{"LT001 unknown object type", withOntology(map[string]string{"a.yaml": aircraftOK, "l.yaml": `linkTypes:
  - apiName: aircraftEngines
    from: { objectType: Aircraft, apiName: engines, cardinality: MANY }
    to: { objectType: Engine, apiName: aircraft, cardinality: ONE }
    foreignKey: { on: Engine, property: tailNumber }
`}), "LT001"},
		{"LT002 navigation conflicts with property", withOntology(map[string]string{"a.yaml": aircraftOK, "l.yaml": `linkTypes:
  - apiName: parentAircraft
    from: { objectType: Aircraft, apiName: baseAirport, cardinality: ONE }
    to: { objectType: Aircraft, apiName: children, cardinality: MANY }
    foreignKey: { on: Aircraft, property: tailNumber }
`}), "LT002"},
		{"LT003 neither fk nor join table", withOntology(map[string]string{"a.yaml": aircraftOK, "l.yaml": `linkTypes:
  - apiName: sisterShips
    from: { objectType: Aircraft, apiName: sisters, cardinality: MANY }
    to: { objectType: Aircraft, apiName: sisterOf, cardinality: MANY }
`}), "LT003"},
		{"LT004 fk property missing", withOntology(map[string]string{"a.yaml": aircraftOK, "l.yaml": `linkTypes:
  - apiName: parentAircraft
    from: { objectType: Aircraft, apiName: parent, cardinality: ONE }
    to: { objectType: Aircraft, apiName: children, cardinality: MANY }
    foreignKey: { on: Aircraft, property: parentTail }
`}), "LT004"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileMap(t, tc.files)
			if !hasCode(diags, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, diags.Codes())
			}
		})
	}
}

// E02: a misspelled valueType reports OT003 at the correct line with a suggestion.
func TestValueTypeTypoPositionAndSuggestion(t *testing.T) {
	wo := `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
    status: { type: string, valueType: WorkorderStatus }
  datasource:
    connector: mro_db
    table: public.work_orders
`
	vt := "valueTypes:\n  - apiName: WorkOrderStatus\n    baseType: string\n"
	_, diags := compileMap(t, withOntology(map[string]string{"wo.yaml": wo, "vt.yaml": vt}))
	var found *Diagnostic
	for i := range diags {
		if diags[i].Code == "OT003" {
			found = &diags[i]
		}
	}
	if found == nil {
		t.Fatalf("expected OT003, got %v", diags.Codes())
	}
	if found.Pos.File != "mem/wo.yaml" || found.Pos.Line != 7 {
		t.Errorf("position = %s, want mem/wo.yaml:7", found.Pos)
	}
	if !strings.Contains(found.Message, `did you mean "WorkOrderStatus"`) {
		t.Errorf("message lacks suggestion: %s", found.Message)
	}
}

func TestInvalidYAMLReportsLine(t *testing.T) {
	_, diags := compileMap(t, withOntology(map[string]string{"bad.yaml": "objectType:\n  apiName: [unclosed\n"}))
	if !hasCode(diags, "G000") {
		t.Fatalf("expected G000, got %v", diags.Codes())
	}
}
