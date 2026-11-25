package expr

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/acme/ontology/internal/spec"
)

// TestActionCriterionAT003 covers the spec↔expr wiring for AT003.
//
// AT003 is emitted only when spec's boolCompiler hook is non-nil, and that hook is registered
// by this package's init (bridge.go). Package spec cannot import expr, so inside spec's own
// test binary the hook is always nil and AT003 is unreachable — hence the assertion lives here.
// TestCompileBool above covers the CEL validation itself; this covers the compiler reporting it.
func TestActionCriterionAT003(t *testing.T) {
	const ontology = `ontology:
  apiName: mro
  displayName: MRO
  connectors:
    mro_db: { kind: postgres, dsnRef: "vault://mro/db" }
`
	const woType = `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
    status: { type: string }
  datasource: { connector: mro_db, table: public.work_orders }
`
	actionType := func(criterion string) string {
		return `actionType:
  apiName: closeWorkOrder
  description: close a work order
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  submissionCriteria: [{ expr: ` + criterion + `, message: not allowed }]
  rules: [{ deleteObject: { object: "{{wo}}" } }]
`
	}

	cases := []struct {
		name      string
		criterion string // a complete YAML scalar
		wantAT003 bool
	}{
		{"syntax error", `'wo.status ='`, true},
		// objectRef variables are dynamic maps (env.go), so a bare `wo.status` is dyn and
		// CompileBool accepts it by design. The expression must be statically non-bool.
		{"statically not a bool", `'wo.priority + 1'`, true},
		{"unknown variable", `'nope.status == "OPEN"'`, true},
		{"dynamic property is accepted", `'wo.status'`, false},
		{"valid bool", `'wo.status == "OPEN"'`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"ontology.yaml": &fstest.MapFile{Data: []byte(ontology)},
				"wo.yaml":       &fstest.MapFile{Data: []byte(woType)},
				"act.yaml":      &fstest.MapFile{Data: []byte(actionType(tc.criterion))},
			}
			loaded, ldiags := spec.LoadFS(fsys, "mem")
			if len(ldiags) != 0 {
				t.Fatalf("unexpected load diagnostics: %v", ldiags)
			}
			_, diags := spec.Compile(loaded)

			codes := diags.Codes()
			if got := slices.Contains(codes, "AT003"); got != tc.wantAT003 {
				t.Fatalf("AT003 present = %v, want %v (all codes: %v)", got, tc.wantAT003, codes)
			}
			// The criterion is the only thing under test; anything else means the fixture
			// drifted and the assertion above is no longer meaningful.
			for _, d := range diags {
				if d.Severity == spec.SevError && d.Code != "AT003" {
					t.Errorf("unexpected error in fixture: %s", d.String())
				}
			}
		})
	}
}
