package action

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/acme/ontology/internal/expr"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

// celVars builds the CEL activation: primitives as-is, objects as property maps, plus `user`.
func celVars(at *spec.ActionType, rp *resolvedParams, objs map[objstore.ObjectRef]*objstore.Object, actor Actor) map[string]any {
	vars := map[string]any{}
	for _, name := range at.ParamOrder {
		p := at.Parameters[name]
		switch p.Type.Kind {
		case spec.KindObjectRef:
			if ref, ok := rp.refs[name]; ok {
				vars[name] = objectMap(objs[ref])
			} else {
				vars[name] = map[string]any{}
			}
		case spec.KindObjectRefList:
			list := []any{}
			for _, ref := range rp.refLists[name] {
				list = append(list, objectMap(objs[ref]))
			}
			vars[name] = list
		default:
			if v, ok := rp.values[name]; ok {
				vars[name] = v
			} else {
				vars[name] = zeroValue(p.Type.Base) // optional and absent: typed zero value
			}
		}
	}
	groups := make([]any, len(actor.Groups))
	for i, g := range actor.Groups {
		groups[i] = g
	}
	vars["user"] = map[string]any{"id": actor.ID, "kind": string(actor.Kind), "groups": groups}
	return vars
}

func objectMap(o *objstore.Object) map[string]any {
	m := map[string]any{}
	if o == nil {
		return m
	}
	for k, v := range o.Props {
		m[k] = v
	}
	return m
}

func paramObject(rp *resolvedParams, objs map[objstore.ObjectRef]*objstore.Object, tmpl string) (*objstore.Object, error) {
	name := spec.ParamRef(tmpl)
	ref, ok := rp.refs[name]
	if !ok {
		return nil, fmt.Errorf("rule references parameter %q, which was not provided", name)
	}
	return objs[ref], nil
}

func renderValue(env *expr.Env, vars map[string]any, tmpl string, prop *spec.Property) (any, error) {
	t := expr.ParseTemplate(tmpl)
	if len(t.Exprs()) == 0 {
		return coerceLiteral(prop, tmpl), nil
	}
	return t.Render(env, vars)
}

// renderSet evaluates a rule's `set:` block; keys are visited in sorted order for determinism.
func renderSet(env *expr.Env, vars map[string]any, ot *spec.ObjectType, set map[string]string) (map[string]any, error) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(set))
	for _, k := range keys {
		prop := ot.Properties[k]
		if prop == nil {
			return nil, fmt.Errorf("unknown property %q on %s", k, ot.APIName)
		}
		v, err := renderValue(env, vars, set[k], prop)
		if err != nil {
			return nil, fmt.Errorf("property %s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// expandRules turns built-in rules into edits (Spec §7.3 step 8).
func expandRules(ont *spec.CompiledOntology, at *spec.ActionType, env *expr.Env, vars map[string]any,
	rp *resolvedParams, objs map[objstore.ObjectRef]*objstore.Object) ([]objstore.Edit, error) {

	fail := func(i int, err error) error {
		return apperr.New(http.StatusUnprocessableEntity, "EDIT_VALIDATION_FAILED", "rule %d: %v", i, err)
	}
	var edits []objstore.Edit
	for i, r := range at.Rules {
		switch r.Kind {
		case spec.RuleModify, spec.RuleDelete:
			o, err := paramObject(rp, objs, r.Object)
			if err != nil {
				return nil, fail(i, err)
			}
			if r.Kind == spec.RuleDelete {
				edits = append(edits, objstore.Edit{Op: objstore.OpDelete, Ref: o.Ref, ExpectedVersion: o.Version})
				continue
			}
			changes, err := renderSet(env, vars, ont.ObjectTypes[o.Ref.Type], r.Set)
			if err != nil {
				return nil, fail(i, err)
			}
			edits = append(edits, objstore.Edit{Op: objstore.OpModify, Ref: o.Ref, Changes: changes, ExpectedVersion: o.Version})
		case spec.RuleCreate:
			ot := ont.ObjectTypes[r.ObjectType]
			if ot == nil {
				return nil, fail(i, fmt.Errorf("unknown object type %q", r.ObjectType))
			}
			changes, err := renderSet(env, vars, ot, r.Set)
			if err != nil {
				return nil, fail(i, err)
			}
			pk, ok := changes[ot.PrimaryKey]
			if !ok || pk == nil || fmtPK(pk) == "" {
				return nil, fail(i, fmt.Errorf("createObject must set primary key %q", ot.PrimaryKey))
			}
			edits = append(edits, objstore.Edit{Op: objstore.OpCreate, Ref: objstore.ObjectRef{Type: ot.APIName, PK: fmtPK(pk)}, Changes: changes})
		case spec.RuleLink, spec.RuleUnlink:
			from, err := paramObject(rp, objs, r.From)
			if err != nil {
				return nil, fail(i, err)
			}
			to, err := paramObject(rp, objs, r.To)
			if err != nil {
				return nil, fail(i, err)
			}
			op := objstore.OpLink
			if r.Kind == spec.RuleUnlink {
				op = objstore.OpUnlink
			}
			edits = append(edits, objstore.Edit{Op: op, LinkType: r.LinkType, Ref: from.Ref, Target: to.Ref, ExpectedVersion: from.Version})
		default:
			return nil, fail(i, fmt.Errorf("unsupported rule kind %q", r.Kind))
		}
	}
	return edits, nil
}
