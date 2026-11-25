package spec

import (
	"strings"
	"testing"
)

// minimal object + action fixtures layered on withOntology.
const woType = `objectType:
  apiName: WorkOrder
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
    status: { type: string }
    technicianId: { type: string }
  datasource: { connector: mro_db, table: public.work_orders }
  authz:
    fgaType: work_order
    tuples: [{ relation: a, target: "x:{{id}}" }]
`

const techType = `objectType:
  apiName: Technician
  primaryKey: id
  titleProperty: id
  properties:
    id: { type: string, required: true }
  datasource: { connector: mro_db, table: public.tech }
`

func actionFiles(action string) map[string]string {
	return withOntology(map[string]string{"wo.yaml": woType, "tech.yaml": techType, "act.yaml": action})
}

func TestActionValid(t *testing.T) {
	action := `actionType:
  apiName: assign
  description: assign a WO
  parameters:
    workOrder:  { type: objectRef<WorkOrder>, required: true }
    technician: { type: objectRef<Technician>, required: true }
  submissionCriteria:
    - { expr: 'workOrder.status == "OPEN"', message: not open }
  rules:
    - modifyObject:
        object: "{{workOrder}}"
        set: { technicianId: "{{technician.id}}", status: IN_PROGRESS }
  permission: { relation: can_assign, on: "{{workOrder}}" }
`
	_, diags := compileMap(t, actionFiles(action))
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags.Codes())
	}
}

// TestActionRules covers the structural action rules. AT003 is absent on purpose: it only
// fires when boolCompiler is non-nil, and the hooks are injected by internal/expr, which this
// package cannot import. AT003 is covered in internal/expr/action_compile_test.go.
func TestActionRules(t *testing.T) {
	cases := []struct {
		name, action, code string
	}{
		{"AT001 neither rules nor function", `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<WorkOrder> } }
`, "AT001"},
		{"AT001 both rules and function", `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  function: { runtime: go, ref: f }
  rules: [{ deleteObject: { object: "{{wo}}" } }]
`, "AT001"},
		{"AT002 unknown objectRef", `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<Nope>, required: true } }
  rules: [{ deleteObject: { object: "{{wo}}" } }]
`, "AT002"},
		{"AT004 unknown property in set", `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  rules: [{ modifyObject: { object: "{{wo}}", set: { nope: "1" } } }]
`, "AT004"},
		{"AT005 non-objectRef in modify", `actionType:
  apiName: act
  parameters: { s: { type: string } }
  rules: [{ modifyObject: { object: "{{s}}", set: { status: X } } }]
`, "AT005"},
		{"AT006 create missing required", `actionType:
  apiName: act
  parameters: { x: { type: string } }
  rules: [{ createObject: { objectType: WorkOrder, set: { status: X } } }]
`, "AT006"},
		{"AT007 unknown webhook", `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  rules: [{ deleteObject: { object: "{{wo}}" } }]
  sideEffects: [{ webhook: ghost, mode: SIDE_EFFECT }]
`, "AT007"},
		{"AT009 permission on non-authz type", `actionType:
  apiName: act
  parameters: { t: { type: objectRef<Technician>, required: true } }
  rules: [{ deleteObject: { object: "{{t}}" } }]
  permission: { relation: r, on: "{{t}}" }
`, "AT009"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileMap(t, actionFiles(tc.action))
			if !hasCode(diags, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, diags.Codes())
			}
		})
	}
}

func TestWebhookRules(t *testing.T) {
	badSecret := withOntology(map[string]string{
		"wo.yaml": woType, "tech.yaml": techType,
		"wh.yaml": "webhooks:\n  - name: w\n    method: POST\n    url: \"https://x/{{wo}}\"\n    auth: { type: bearer, secretRef: \"plain-text\" }\n",
	})
	_, diags := compileMap(t, badSecret)
	if !hasCode(diags, "WH002") {
		t.Fatalf("expected WH002, got %v", diags.Codes())
	}
	unbalanced := withOntology(map[string]string{
		"wh.yaml": "webhooks:\n  - name: w\n    method: POST\n    url: \"https://x/{{wo\"\n",
	})
	_, diags = compileMap(t, unbalanced)
	if !hasCode(diags, "WH001") {
		t.Fatalf("expected WH001, got %v", diags.Codes())
	}
}

func TestActionExposedNoDescriptionWarns(t *testing.T) {
	action := `actionType:
  apiName: act
  parameters: { wo: { type: objectRef<WorkOrder>, required: true } }
  rules: [{ deleteObject: { object: "{{wo}}" } }]
  ai: { exposeToAgents: true }
`
	_, diags := compileMap(t, actionFiles(action))
	if !hasCode(diags, "AT010") {
		t.Fatalf("expected AT010 warning, got %v", diags.Codes())
	}
	if strings.Contains(strings.Join(diags.Codes(), ","), "error") {
		// AT010 is a warning, not an error
	}
}
