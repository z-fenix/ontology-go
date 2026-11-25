package action

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

type resolvedParams struct {
	values   map[string]any                  // primitive parameters (coerced, defaults applied)
	refs     map[string]objstore.ObjectRef   // objectRef parameters
	refLists map[string][]objstore.ObjectRef // objectRefList parameters
}

func (r *resolvedParams) allRefs() []objstore.ObjectRef {
	seen := map[objstore.ObjectRef]bool{}
	var out []objstore.ObjectRef
	add := func(ref objstore.ObjectRef) {
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, ref := range r.refs {
		add(ref)
	}
	for _, list := range r.refLists {
		for _, ref := range list {
			add(ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

type paramProblem struct {
	Code      string `json:"code"`
	Parameter string `json:"parameter"`
	Message   string `json:"message"`
}

func toPK(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "", fmt.Errorf("object reference must not be empty")
		}
		return x, nil
	case map[string]any: // {"pk": "..."} form
		if pk, ok := x["pk"]; ok {
			return toPK(pk)
		}
	}
	if n, ok := toInt(v); ok {
		return fmtPK(n), nil
	}
	return "", fmt.Errorf("object reference must be a primary key string, got %T", v)
}

// resolveParams validates raw parameters against the action type (Spec §7.3 step 3).
// All problems are collected and returned together.
func resolveParams(ont *spec.CompiledOntology, at *spec.ActionType, raw map[string]any) (*resolvedParams, error) {
	rp := &resolvedParams{values: map[string]any{}, refs: map[string]objstore.ObjectRef{}, refLists: map[string][]objstore.ObjectRef{}}
	var problems []paramProblem
	add := func(code, name, format string, args ...any) {
		problems = append(problems, paramProblem{Code: code, Parameter: name, Message: fmt.Sprintf(format, args...)})
	}
	unknown := make([]string, 0)
	for name := range raw {
		if _, ok := at.Parameters[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		add("UNKNOWN_PARAMETER", name, "unknown parameter %q", name)
	}

	for _, name := range at.ParamOrder {
		p := at.Parameters[name]
		v, present := raw[name]
		if (!present || v == nil) && p.Default != nil {
			v, present = p.Default, true
		}
		if !present || v == nil {
			if p.Required {
				add("MISSING_PARAMETER", name, "parameter %q is required", name)
			}
			continue
		}
		switch p.Type.Kind {
		case spec.KindObjectRef:
			pk, err := toPK(v)
			if err != nil {
				add("INVALID_PARAMETER", name, "%v", err)
				continue
			}
			rp.refs[name] = objstore.ObjectRef{Type: p.Type.ObjectType, PK: pk}
		case spec.KindObjectRefList:
			items, ok := v.([]any)
			if !ok {
				add("INVALID_PARAMETER", name, "expected a list of object references")
				continue
			}
			refs := make([]objstore.ObjectRef, 0, len(items))
			for i, item := range items {
				pk, err := toPK(item)
				if err != nil {
					add("INVALID_PARAMETER", name, "item %d: %v", i, err)
					continue
				}
				refs = append(refs, objstore.ObjectRef{Type: p.Type.ObjectType, PK: pk})
			}
			rp.refLists[name] = refs
		default:
			cv, err := coerce(p.Type.Base, v)
			if err != nil {
				add("INVALID_PARAMETER", name, "%v", err)
				continue
			}
			prop := &spec.Property{Constraints: p.Constraints, ValueType: p.ValueType}
			if err := checkProperty(ont, prop, cv); err != nil {
				add("INVALID_PARAMETER", name, "%v", err)
				continue
			}
			rp.values[name] = cv
		}
	}
	if len(problems) > 0 {
		first := problems[0]
		return nil, apperr.New(http.StatusBadRequest, first.Code, "%s", first.Message).
			WithDetails(map[string]any{"problems": problems})
	}
	return rp, nil
}
