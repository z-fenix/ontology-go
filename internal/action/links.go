package action

import (
	"context"
	"fmt"
	"net/http"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

// fkValue returns the primary key referenced by the object's foreign-key property.
func fkValue(o *objstore.Object, prop string) (string, bool) {
	if o == nil {
		return "", false
	}
	v := o.Props[prop]
	if v == nil {
		return "", false
	}
	s := fmt.Sprint(v)
	return s, s != ""
}

// resolveHop finds the object one single-valued link away from o, or nil when it is unset,
// missing, or not visible to the actor (an invisible object is indistinguishable from none).
func (e *Engine) resolveHop(ctx context.Context, ont *spec.CompiledOntology, actor Actor, o *objstore.Object, nav *spec.Navigation) (*objstore.Object, error) {
	lt := nav.Link
	var ref objstore.ObjectRef
	switch {
	case lt.ForeignKey != nil && nav.FromSide == objstore.FKHolderIsFrom(lt):
		pk, ok := fkValue(o, lt.ForeignKey.Property) // this object holds the key
		if !ok {
			return nil, nil
		}
		ref = objstore.ObjectRef{Type: nav.Target, PK: pk}
	case lt.ForeignKey != nil:
		// The target holds the key and the navigation is single-valued (1:1 seen from the other side).
		objs, _, err := e.Objects.Find(ctx, ont, nav.Target, objstore.Filter{Eq: map[string]any{lt.ForeignKey.Property: o.Ref.PK}}, objstore.Page{Size: 2})
		if err != nil || len(objs) == 0 {
			return nil, err
		}
		if len(objs) > 1 {
			return nil, apperr.New(http.StatusUnprocessableEntity, "EDIT_VALIDATION_FAILED",
				"link %s from %s is declared single-valued but matches several objects", lt.APIName, o.Ref)
		}
		ref = objs[0].Ref
	default:
		return nil, fmt.Errorf("link %s cannot be followed in expressions", lt.APIName)
	}
	found, err := e.Objects.GetMany(ctx, ont, []objstore.ObjectRef{ref})
	if err != nil {
		return nil, err
	}
	target := found[ref]
	if target == nil {
		return nil, nil
	}
	if ot := ont.ObjectTypes[ref.Type]; ot != nil && ot.Authz != nil {
		ok, err := e.allowed(ctx, actor, "viewer", ot.Authz.FGAType+":"+ref.PK)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}
	return target, nil
}

// attachLinks loads every link path the action's expressions use and nests the linked objects
// into the CEL variables, so `workOrder.aircraft.model` evaluates against real data. A link
// with no visible target is set to null: `has(workOrder.aircraft)` style checks, or comparisons
// that fail on null, decide what happens — the action is not rejected just for an empty link.
func (e *Engine) attachLinks(ctx context.Context, ont *spec.CompiledOntology, at *spec.ActionType, actor Actor,
	rp *resolvedParams, objs map[objstore.ObjectRef]*objstore.Object, vars map[string]any) error {

	paths, err := ont.ActionLinkPaths(at)
	if err != nil {
		return apperr.New(http.StatusInternalServerError, "INTERNAL", "action %s has invalid link paths: %v", at.APIName, err)
	}
	type node struct {
		obj  *objstore.Object
		vars map[string]any
	}
	resolved := map[string]*node{}
	for _, lp := range paths { // sorted by hop count, so a prefix is always resolved first
		parentKey := spec.LinkPath{Param: lp.Param, Hops: lp.Hops[:len(lp.Hops)-1]}.Key()
		parent := resolved[parentKey]
		if parent == nil && len(lp.Hops) == 1 {
			ref, ok := rp.refs[lp.Param]
			if !ok {
				continue
			}
			m, _ := vars[lp.Param].(map[string]any)
			parent = &node{obj: objs[ref], vars: m}
			resolved[parentKey] = parent
		}
		if parent == nil || parent.obj == nil || parent.vars == nil {
			continue // an earlier hop was empty
		}
		hop := lp.Hops[len(lp.Hops)-1]
		nav, ok := ont.Navigate(parent.obj.Ref.Type, hop)
		if !ok {
			continue
		}
		target, err := e.resolveHop(ctx, ont, actor, parent.obj, nav)
		if err != nil {
			return err
		}
		if target == nil {
			parent.vars[hop] = nil
			resolved[lp.Key()] = &node{}
			continue
		}
		m := objectMap(target)
		parent.vars[hop] = m
		resolved[lp.Key()] = &node{obj: target, vars: m}
	}
	return nil
}
