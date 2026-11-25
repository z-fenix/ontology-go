package spec

import "testing"

func base() *CompiledOntology {
	return &CompiledOntology{
		APIName:    "mro",
		ValueTypes: map[string]*ValueType{},
		ObjectTypes: map[string]*ObjectType{
			"WorkOrder": {
				APIName: "WorkOrder", PrimaryKey: "id", PropertyOrder: []string{"id", "priority"},
				Properties: map[string]*Property{
					"id":       {APIName: "id", Type: TypeRef{Base: TString}, Required: true},
					"priority": {APIName: "priority", Type: TypeRef{Base: TInteger}},
				},
			},
		},
		LinkTypes:   map[string]*LinkType{},
		ActionTypes: map[string]*ActionType{},
		Webhooks:    map[string]*Webhook{},
	}
}

func kinds(cs ChangeSet) map[ChangeKind]Change {
	m := map[ChangeKind]Change{}
	for _, c := range cs.Changes {
		m[c.Kind] = c
	}
	return m
}

func TestDiffFirstPublishNonBreaking(t *testing.T) {
	cs := Diff(nil, base())
	if cs.HasBreaking() {
		t.Error("first publish should have no breaking changes")
	}
	if _, ok := kinds(cs)[AddObjectType]; !ok {
		t.Error("expected add_object_type")
	}
}

func TestDiffAddOptionalProperty(t *testing.T) {
	old := base()
	cur := base()
	wo := cur.ObjectTypes["WorkOrder"]
	wo.Properties["note"] = &Property{APIName: "note", Type: TypeRef{Base: TString}}
	wo.PropertyOrder = append(wo.PropertyOrder, "note")
	cs := Diff(old, cur)
	if cs.HasBreaking() {
		t.Errorf("adding optional property should be non-breaking: %+v", cs.Changes)
	}
}

func TestDiffBreakingChanges(t *testing.T) {
	old := base()

	dropProp := base()
	delete(dropProp.ObjectTypes["WorkOrder"].Properties, "priority")
	dropProp.ObjectTypes["WorkOrder"].PropertyOrder = []string{"id"}
	if !Diff(old, dropProp).HasBreaking() {
		t.Error("dropping a property must be breaking")
	}

	addReq := base()
	wo := addReq.ObjectTypes["WorkOrder"]
	wo.Properties["owner"] = &Property{APIName: "owner", Type: TypeRef{Base: TString}, Required: true}
	wo.PropertyOrder = append(wo.PropertyOrder, "owner")
	cs := Diff(old, addReq)
	if k, ok := kinds(cs)[AddRequiredProp]; !ok || !k.Breaking {
		t.Error("adding a required property must be breaking")
	}

	changePK := base()
	changePK.ObjectTypes["WorkOrder"].PrimaryKey = "priority"
	if k, ok := kinds(Diff(old, changePK))[ChangePrimaryKey]; !ok || !k.Breaking {
		t.Error("changing the primary key must be breaking")
	}

	castType := base()
	castType.ObjectTypes["WorkOrder"].Properties["priority"].Type = TypeRef{Base: TString}
	if k, ok := kinds(Diff(old, castType))[ChangePropertyType]; !ok || !k.Breaking {
		t.Error("changing a property type must be breaking")
	}
}

func TestDiffTightenEnum(t *testing.T) {
	old := base()
	old.ObjectTypes["WorkOrder"].Properties["priority"].Constraints = Constraints{Enum: []string{"1", "2", "3"}}
	cur := base()
	cur.ObjectTypes["WorkOrder"].Properties["priority"].Constraints = Constraints{Enum: []string{"1", "2"}}
	if k, ok := kinds(Diff(old, cur))[TightenConstraint]; !ok || !k.Breaking {
		t.Error("removing an enum value must be a breaking tighten")
	}
	// widening the enum is not breaking
	cur2 := base()
	cur2.ObjectTypes["WorkOrder"].Properties["priority"].Constraints = Constraints{Enum: []string{"1", "2", "3", "4"}}
	if kinds(Diff(old, cur2))[TightenConstraint].Breaking {
		t.Error("adding an enum value should not be breaking")
	}
}

func TestMigrationCoverage(t *testing.T) {
	old := base()
	cur := base()
	delete(cur.ObjectTypes["WorkOrder"].Properties, "priority")
	cur.ObjectTypes["WorkOrder"].PropertyOrder = []string{"id"}
	cs := Diff(old, cur)

	missing := MigrationCoverage(cs, nil)
	if len(missing) != 1 || missing[0].Kind != DropProperty {
		t.Fatalf("expected 1 uncovered drop, got %+v", missing)
	}
	covered := MigrationCoverage(cs, []Migration{
		{Type: MigDropProperty, ObjectType: "WorkOrder", Property: "priority"},
	})
	if len(covered) != 0 {
		t.Errorf("drop property should be covered, got %+v", covered)
	}
}
