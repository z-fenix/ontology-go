package authz

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/acme/ontology/internal/spec"
)

// Model is the subset of an OpenFGA authorization model (JSON syntax) that the checks read.
type Model struct {
	SchemaVersion   string    `json:"schema_version"`
	TypeDefinitions []TypeDef `json:"type_definitions"`
}

// TypeDef is one type definition.
type TypeDef struct {
	Type      string                     `json:"type"`
	Relations map[string]json.RawMessage `json:"relations,omitempty"`
	Metadata  *struct {
		Relations map[string]struct {
			DirectlyRelatedUserTypes []struct {
				Type     string `json:"type"`
				Relation string `json:"relation,omitempty"`
			} `json:"directly_related_user_types"`
		} `json:"relations"`
	} `json:"metadata,omitempty"`
}

// ParseModel decodes an authorization model in OpenFGA JSON syntax.
func ParseModel(b []byte) (*Model, error) {
	var m Model
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("authorization model: %w", err)
	}
	if m.SchemaVersion == "" || len(m.TypeDefinitions) == 0 {
		return nil, fmt.Errorf("authorization model: schema_version and type_definitions are required")
	}
	return &m, nil
}

func (m *Model) typeDef(name string) *TypeDef {
	for i := range m.TypeDefinitions {
		if m.TypeDefinitions[i].Type == name {
			return &m.TypeDefinitions[i]
		}
	}
	return nil
}

// directTypes lists the user types allowed to be written directly for relation on typ
// ("base", "user", "group#member"). nil means the relation is not directly assignable.
func (m *Model) directTypes(typ, relation string) []string {
	td := m.typeDef(typ)
	if td == nil || td.Metadata == nil {
		return nil
	}
	r, ok := td.Metadata.Relations[relation]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(r.DirectlyRelatedUserTypes))
	for _, t := range r.DirectlyRelatedUserTypes {
		if t.Relation != "" {
			out = append(out, t.Type+"#"+t.Relation)
		} else {
			out = append(out, t.Type)
		}
	}
	return out
}

// Problem is one mismatch between the ontology and the authorization model.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// CheckModel verifies that the ontology only uses types and relations the model defines:
// every authz fgaType exists, every derived tuple targets a directly assignable relation whose
// allowed user types include the template's type, every relation the services check exists.
func CheckModel(ont *spec.CompiledOntology, m *Model) []Problem {
	var out []Problem
	add := func(path, format string, args ...any) {
		out = append(out, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasRel := func(typ, rel string) bool {
		td := m.typeDef(typ)
		if td == nil {
			return false
		}
		_, ok := td.Relations[rel]
		return ok
	}
	for _, name := range sortedKeys(ont.ObjectTypes) {
		ot := ont.ObjectTypes[name]
		if ot.Authz == nil {
			continue
		}
		path := "objectTypes." + name + ".authz"
		fga := ot.Authz.FGAType
		if m.typeDef(fga) == nil {
			add(path+".fgaType", "type %q is not defined in the authorization model", fga)
			continue
		}
		if !hasRel(fga, "viewer") {
			add(path+".fgaType", "type %q has no viewer relation (required for object visibility)", fga)
		}
		for i, t := range ot.Authz.Tuples {
			tp := fmt.Sprintf("%s.tuples[%d]", path, i)
			if !hasRel(fga, t.Relation) {
				add(tp, "relation %q is not defined on type %q", t.Relation, fga)
				continue
			}
			userType, _, ok := strings.Cut(t.Target, ":")
			if !ok || userType == "" || strings.Contains(userType, "{{") {
				add(tp, "target %q must start with a fixed type, e.g. \"user:{{id}}\"", t.Target)
				continue
			}
			allowed := m.directTypes(fga, t.Relation)
			if allowed == nil {
				add(tp, "relation %s#%s cannot be written directly (no directly related user types)", fga, t.Relation)
				continue
			}
			if !contains(allowed, userType) {
				add(tp, "relation %s#%s does not accept %q (allowed: %s)", fga, t.Relation, userType, strings.Join(allowed, ", "))
			}
		}
	}
	for _, name := range sortedKeys(ont.ActionTypes) {
		at := ont.ActionTypes[name]
		if at.Permission == nil {
			continue
		}
		p := at.Parameters[spec.ParamRef(at.Permission.On)]
		if p == nil {
			continue // reported by the ontology compiler (AT009)
		}
		ot := ont.ObjectTypes[p.Type.ObjectType]
		if ot == nil || ot.Authz == nil {
			continue
		}
		if !hasRel(ot.Authz.FGAType, at.Permission.Relation) {
			add("actionTypes."+name+".permission", "relation %q is not defined on type %q", at.Permission.Relation, ot.Authz.FGAType)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
