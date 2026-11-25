package spec

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func exampleOntology(t *testing.T) *CompiledOntology {
	t.Helper()
	files, ld := LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example: %v", d.Codes())
	}
	return ont
}

func TestSelectorChains(t *testing.T) {
	got := SelectorChains(`wo.aircraft.model == "a.b" && user.id != "" && size(note) > 0 && wo . status == 'x.y'`)
	want := [][]string{{"wo", "aircraft", "model"}, {"user", "id"}, {"wo", "status"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chains = %v", got)
	}
	tpl := SelectorChains("https://x/{{ wo.aircraft.tailNumber }}/y.json?{{note}}")
	if !reflect.DeepEqual(tpl, [][]string{{"wo", "aircraft", "tailNumber"}}) {
		t.Errorf("template chains = %v", tpl)
	}
}

func TestResolveLinkPaths(t *testing.T) {
	ont := exampleOntology(t)
	at := &ActionType{Parameters: map[string]*ActionParam{
		"wo":   {Type: ParamType{Kind: KindObjectRef, ObjectType: "WorkOrder"}},
		"note": {Type: ParamType{Kind: KindPrimitive, Base: TString}},
	}}
	paths, err := ont.ResolveLinkPaths(at, `wo.aircraft.model == "A320" && wo.technician.name != "" && wo.summary.startsWith("x")`)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, p := range paths {
		keys = append(keys, p.Key())
	}
	if strings.Join(keys, ",") != "wo.aircraft,wo.technician" {
		t.Errorf("paths = %v", keys)
	}
	bad := map[string]string{
		`wo.aircraft.workOrders.size() > 0`: "many objects",
		`wo.aircrft.model == ""`:            `did you mean "aircraft"`,
	}
	for src, want := range bad {
		if _, err := ont.ResolveLinkPaths(at, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	if p, err := ont.ResolveLinkPaths(at, `note == "a" && user.id == "b"`); err != nil || len(p) != 0 {
		t.Errorf("non-object chains: %v, %v", p, err)
	}
}

func TestAT011(t *testing.T) {
	act := `actionType:
  apiName: check
  description: check
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  submissionCriteria: [{ expr: 'wo.aircraft.workOrders.size() > 0', message: x }]
  rules: [{ modifyObject: { object: "{{wo}}", set: { status: DONE } } }]
`
	wo := `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
    status: { type: string }
    tail: { type: string }
  datasource: { connector: mro_db, table: public.wo }
`
	ac := `objectType:
  apiName: Aircraft
  primaryKey: tail
  titleProperty: tail
  properties:
    tail: { type: string, required: true }
  datasource: { connector: mro_db, table: public.ac }
`
	links := `linkTypes:
  - apiName: aircraftWorkOrders
    from: { objectType: Aircraft, apiName: workOrders, cardinality: MANY }
    to: { objectType: WorkOrder, apiName: aircraft, cardinality: ONE }
    foreignKey: { on: WorkOrder, property: tail }
`
	_, diags := compileMap(t, withOntology(map[string]string{"wo.yaml": wo, "ac.yaml": ac, "l.yaml": links, "a.yaml": act}))
	if !hasCode(diags, "AT011") {
		t.Fatalf("expected AT011, got %v", diags.Codes())
	}
}

func TestRenderTupleAndTupleProps(t *testing.T) {
	if s, ok := RenderTuple("base:{{baseAirport}}", map[string]any{"baseAirport": "SIN"}); !ok || s != "base:SIN" {
		t.Errorf("render = %q %v", s, ok)
	}
	if _, ok := RenderTuple("user:{{technicianId}}", map[string]any{"technicianId": nil}); ok {
		t.Error("a null property must not yield a tuple")
	}
	ont := exampleOntology(t)
	if got := TupleProps(ont.ObjectTypes["WorkOrder"]); !reflect.DeepEqual(got, []string{"tailNumber", "technicianId"}) {
		t.Errorf("tuple props = %v", got)
	}
}
