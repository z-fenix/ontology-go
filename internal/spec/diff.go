package spec

import (
	"fmt"
	"sort"
)

// ChangeKind categorizes a single difference between two ontology versions.
type ChangeKind string

const (
	AddObjectType      ChangeKind = "add_object_type"
	DropObjectTypeC    ChangeKind = "drop_object_type"
	AddProperty        ChangeKind = "add_property"
	AddRequiredProp    ChangeKind = "add_required_property"
	DropProperty       ChangeKind = "drop_property"
	ChangePropertyType ChangeKind = "change_property_type"
	ChangePrimaryKey   ChangeKind = "change_primary_key"
	TightenConstraint  ChangeKind = "tighten_constraint"
	AddLinkType        ChangeKind = "add_link_type"
	DropLinkType       ChangeKind = "drop_link_type"
	AddActionType      ChangeKind = "add_action_type"
	DropActionType     ChangeKind = "drop_action_type"
	ChangeActionParams ChangeKind = "change_action_params"
	AddValueType       ChangeKind = "add_value_type"
	DropValueType      ChangeKind = "drop_value_type"
)

// Change is one difference. Breaking changes require a matching migration to publish.
type Change struct {
	Kind     ChangeKind `json:"kind"`
	Path     string     `json:"path"`
	Breaking bool       `json:"breaking"`
	Detail   string     `json:"detail,omitempty"`
}

// ChangeSet is the ordered result of a diff.
type ChangeSet struct {
	Changes []Change `json:"changes"`
}

// HasBreaking reports whether any change is breaking.
func (cs ChangeSet) HasBreaking() bool {
	for _, c := range cs.Changes {
		if c.Breaking {
			return true
		}
	}
	return false
}

// numericWidening lists allowed widening casts (non-breaking when applied as a cast migration).
var numericWidening = map[BaseType]map[BaseType]bool{
	TInteger: {TLong: true, TDouble: true, TDecimal: true, TString: true},
	TLong:    {TDouble: true, TDecimal: true, TString: true},
	TDouble:  {TString: true},
	TDecimal: {TString: true},
}

// Diff compares two compiled ontologies (old may be nil for a first publish).
func Diff(old, new *CompiledOntology) ChangeSet {
	var cs ChangeSet
	add := func(k ChangeKind, path string, breaking bool, detail string) {
		cs.Changes = append(cs.Changes, Change{Kind: k, Path: path, Breaking: breaking, Detail: detail})
	}
	if old == nil {
		old = &CompiledOntology{}
	}

	diffMapKeys(old.ValueTypes, new.ValueTypes,
		func(name string) { add(AddValueType, "valueTypes."+name, false, "") },
		func(name string) { add(DropValueType, "valueTypes."+name, true, "value type removed") })

	// Object types.
	for _, name := range sortedUnion(keys(old.ObjectTypes), keys(new.ObjectTypes)) {
		o, no := old.ObjectTypes[name], new.ObjectTypes[name]
		path := "objectTypes." + name
		switch {
		case o == nil:
			add(AddObjectType, path, false, "")
		case no == nil:
			add(DropObjectTypeC, path, true, "object type removed")
		default:
			diffObjectType(o, no, path, add)
		}
	}

	diffMapKeys(old.LinkTypes, new.LinkTypes,
		func(name string) { add(AddLinkType, "linkTypes."+name, false, "") },
		func(name string) { add(DropLinkType, "linkTypes."+name, true, "link type removed") })

	// Action types: add/drop non-breaking (actions hold no stored data); param changes are a warning-level change.
	for _, name := range sortedUnion(keys(old.ActionTypes), keys(new.ActionTypes)) {
		o, no := old.ActionTypes[name], new.ActionTypes[name]
		path := "actionTypes." + name
		switch {
		case o == nil:
			add(AddActionType, path, false, "")
		case no == nil:
			add(DropActionType, path, false, "action type removed")
		default:
			if !sameParams(o, no) {
				add(ChangeActionParams, path, false, "action parameters changed")
			}
		}
	}
	return cs
}

func diffObjectType(o, no *ObjectType, path string, add func(ChangeKind, string, bool, string)) {
	if o.PrimaryKey != no.PrimaryKey {
		add(ChangePrimaryKey, path+".primaryKey", true,
			fmt.Sprintf("primary key changed from %q to %q", o.PrimaryKey, no.PrimaryKey))
	}
	for _, pname := range sortedUnion(o.PropertyOrder, no.PropertyOrder) {
		op, np := o.Properties[pname], no.Properties[pname]
		pp := path + ".properties." + pname
		switch {
		case op == nil:
			if np.Required {
				add(AddRequiredProp, pp, true, "new required property needs a backfill migration or default")
			} else {
				add(AddProperty, pp, false, "")
			}
		case np == nil:
			add(DropProperty, pp, true, "property removed")
		default:
			if op.Type.String() != np.Type.String() {
				breaking := true
				detail := fmt.Sprintf("type changed %s -> %s", op.Type, np.Type)
				if numericWidening[op.Type.Base][np.Type.Base] {
					detail += " (widening; needs castProperty migration)"
				}
				add(ChangePropertyType, pp, breaking, detail)
			}
			if tightened(op.Constraints, np.Constraints) {
				add(TightenConstraint, pp, true, "constraint tightened")
			}
		}
	}
}

// tightened reports whether new constraints reject values the old ones accepted (conservative).
func tightened(o, n Constraints) bool {
	if o.Regex == nil && n.Regex != nil {
		return true
	}
	if n.Max != nil && (o.Max == nil || *n.Max < *o.Max) {
		return true
	}
	if n.Min != nil && (o.Min == nil || *n.Min > *o.Min) {
		return true
	}
	if n.MaxLen != nil && (o.MaxLen == nil || *n.MaxLen < *o.MaxLen) {
		return true
	}
	if len(o.Enum) > 0 && len(n.Enum) > 0 {
		set := map[string]bool{}
		for _, e := range n.Enum {
			set[e] = true
		}
		for _, e := range o.Enum {
			if !set[e] {
				return true // a previously allowed value is no longer permitted
			}
		}
	}
	return false
}

func sameParams(o, no *ActionType) bool {
	if len(o.Parameters) != len(no.Parameters) {
		return false
	}
	for name, p := range o.Parameters {
		q, ok := no.Parameters[name]
		if !ok || p.Type != q.Type || p.Required != q.Required {
			return false
		}
	}
	return true
}

func diffMapKeys[V any](old, new map[string]V, onAdd, onDrop func(string)) {
	for _, k := range sortedUnion(keys(old), keys(new)) {
		_, inOld := old[k]
		_, inNew := new[k]
		switch {
		case !inOld:
			onAdd(k)
		case !inNew:
			onDrop(k)
		}
	}
}

func sortedUnion(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// MigrationCoverage reports breaking changes that lack a declared migration.
// This enforces Spec §3.6 / PUB002 at publish time.
func MigrationCoverage(cs ChangeSet, migrations []Migration) []Change {
	covered := func(c Change) bool {
		for _, m := range migrations {
			switch c.Kind {
			case DropProperty:
				if m.Type == MigDropProperty && pathHasProp(c.Path, m.ObjectType, m.Property) {
					return true
				}
			case ChangePropertyType:
				if m.Type == MigCastProperty && pathHasProp(c.Path, m.ObjectType, m.Property) {
					return true
				}
			case AddRequiredProp:
				if (m.Type == MigBackfill) && pathHasProp(c.Path, m.ObjectType, m.Property) {
					return true
				}
			case DropObjectTypeC:
				if m.Type == MigDropObjectType && m.ObjectType != "" && c.Path == "objectTypes."+m.ObjectType {
					return true
				}
			case ChangePrimaryKey:
				return false // never coverable in MVP
			case TightenConstraint, DropValueType, DropLinkType:
				return false // require explicit flag, handled by caller
			}
		}
		return false
	}
	var missing []Change
	for _, c := range cs.Changes {
		if c.Breaking && !covered(c) {
			missing = append(missing, c)
		}
	}
	return missing
}

func pathHasProp(path, objType, prop string) bool {
	return objType != "" && prop != "" &&
		path == "objectTypes."+objType+".properties."+prop
}
