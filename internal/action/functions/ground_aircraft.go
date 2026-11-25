// Package functions holds Go implementations of function-backed action types.
package functions

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/acme/ontology/internal/action"
	"github.com/acme/ontology/internal/objstore"
)

// GroundAircraftRef is the function ref used in action type YAML.
const GroundAircraftRef = "groundAircraft"

// GroundAircraft grounds an aircraft, opens an inspection work order, and raises the priority
// of the aircraft's open work orders. Spec E14: all edits commit in one transaction.
func GroundAircraft(ctx context.Context, in action.FuncInput) ([]objstore.Edit, error) {
	ac := in.Objects["aircraft"]
	if ac == nil {
		return nil, fmt.Errorf("aircraft parameter is required")
	}
	reason, _ := in.Params["reason"].(string)
	edits := []objstore.Edit{{
		Op: objstore.OpModify, Ref: ac.Ref, ExpectedVersion: ac.Version,
		Changes: map[string]any{"status": "GROUNDED"},
	}}
	open, err := in.Reader.Find(ctx, "WorkOrder",
		objstore.Filter{Eq: map[string]any{"tailNumber": ac.Ref.PK, "status": "OPEN"}}, 1000)
	if err != nil {
		return nil, err
	}
	for _, wo := range open {
		edits = append(edits, objstore.Edit{Op: objstore.OpModify, Ref: wo.Ref, ExpectedVersion: wo.Version,
			Changes: map[string]any{"priority": int64(1)}})
	}
	id := "WO-" + uuid.NewString()
	edits = append(edits, objstore.Edit{Op: objstore.OpCreate, Ref: objstore.ObjectRef{Type: "WorkOrder", PK: id},
		Changes: map[string]any{
			"workOrderId": id,
			"summary":     "停场检查：" + reason,
			"status":      "OPEN",
			"priority":    int64(1),
			"tailNumber":  ac.Ref.PK,
		}})
	return edits, nil
}

// Register adds all Go functions to r.
func Register(r *action.FunctionRegistry) {
	r.Register(GroundAircraftRef, GroundAircraft)
}
