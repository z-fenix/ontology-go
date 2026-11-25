package spec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MaxLinkHops bounds how many links an action expression may traverse (Spec §3.5).
const MaxLinkHops = 3

// Navigation is one side of a link as seen from an object type.
type Navigation struct {
	Link     *LinkType
	FromSide bool   // the object type is the link's From side
	Target   string // object type reached
	Many     bool   // the navigation yields many objects
}

// Navigate resolves navigation name nav on object type typ.
func (o *CompiledOntology) Navigate(typ, nav string) (*Navigation, bool) {
	names := make([]string, 0, len(o.LinkTypes))
	for n := range o.LinkTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		lt := o.LinkTypes[n]
		if lt.From.ObjectType == typ && lt.From.APIName == nav {
			return &Navigation{Link: lt, FromSide: true, Target: lt.To.ObjectType, Many: lt.From.Cardinality == CardMany}, true
		}
		if lt.To.ObjectType == typ && lt.To.APIName == nav {
			return &Navigation{Link: lt, FromSide: false, Target: lt.From.ObjectType, Many: lt.To.Cardinality == CardMany}, true
		}
	}
	return nil, false
}

// NavigationNames lists the navigation names available on typ (for suggestions).
func (o *CompiledOntology) NavigationNames(typ string) []string {
	var out []string
	for _, lt := range o.LinkTypes {
		if lt.From.ObjectType == typ {
			out = append(out, lt.From.APIName)
		}
		if lt.To.ObjectType == typ {
			out = append(out, lt.To.APIName)
		}
	}
	sort.Strings(out)
	return out
}

var (
	reStringLit = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
	reSelector  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\s*\.\s*[A-Za-z_][A-Za-z0-9_]*)+`)
)

// SelectorChains returns the dotted selector chains in a CEL expression or template, outside
// string literals, as identifier lists — e.g. "wo.aircraft.model" → [wo aircraft model].
func SelectorChains(src string) [][]string {
	if strings.Contains(src, "{{") {
		var parts []string
		for _, m := range regexp.MustCompile(`\{\{(.*?)\}\}`).FindAllStringSubmatch(src, -1) {
			parts = append(parts, m[1])
		}
		src = strings.Join(parts, " ; ")
	}
	src = reStringLit.ReplaceAllString(src, `""`)
	var out [][]string
	for _, loc := range reSelector.FindAllStringIndex(src, -1) {
		if loc[0] > 0 && src[loc[0]-1] == '.' {
			continue // tail of a chain that starts with a call, e.g. f(x).y
		}
		fields := strings.Split(src[loc[0]:loc[1]], ".")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		out = append(out, fields)
	}
	return out
}

// LinkPath is the link part of a parameter selector chain, e.g. wo.aircraft.model →
// {Param: wo, Hops: [aircraft]}. Hops is empty for a direct property access.
type LinkPath struct {
	Param string
	Hops  []string
}

// Key identifies the path ("wo.aircraft").
func (p LinkPath) Key() string { return strings.Join(append([]string{p.Param}, p.Hops...), ".") }

// ResolveLinkPaths walks every selector chain rooted at an objectRef parameter and returns the
// link paths it needs, failing on unknown names, many-valued links or more than MaxLinkHops.
// Chains not rooted at an objectRef parameter (user.id, primitive parameters) are ignored.
func (o *CompiledOntology) ResolveLinkPaths(at *ActionType, src string) ([]LinkPath, error) {
	var out []LinkPath
	seen := map[string]bool{}
	for _, chain := range SelectorChains(src) {
		p := at.Parameters[chain[0]]
		if p == nil || p.Type.Kind != KindObjectRef {
			continue
		}
		typ := p.Type.ObjectType
		var hops []string
		for _, name := range chain[1:] {
			ot := o.ObjectTypes[typ]
			if ot == nil {
				break
			}
			if _, isProp := ot.Properties[name]; isProp {
				break // a property; anything after it is CEL (e.g. a method call such as .startsWith)
			}
			nav, ok := o.Navigate(typ, name)
			if !ok {
				cands := append(append([]string{}, ot.PropertyOrder...), o.NavigationNames(typ)...)
				return nil, fmt.Errorf("%s: %s has no property or link %q%s", strings.Join(chain, "."), typ, name, didYouMean(name, cands))
			}
			if nav.Many {
				return nil, fmt.Errorf("%s: link %s.%s yields many objects; expressions can only follow single-valued links", strings.Join(chain, "."), typ, name)
			}
			hops = append(hops, name)
			if len(hops) > MaxLinkHops {
				return nil, fmt.Errorf("%s: at most %d link hops are allowed", strings.Join(chain, "."), MaxLinkHops)
			}
			typ = nav.Target
		}
		for i := 1; i <= len(hops); i++ { // every prefix must be loaded
			lp := LinkPath{Param: chain[0], Hops: append([]string(nil), hops[:i]...)}
			if !seen[lp.Key()] {
				seen[lp.Key()] = true
				out = append(out, lp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Hops) != len(out[j].Hops) {
			return len(out[i].Hops) < len(out[j].Hops)
		}
		return out[i].Key() < out[j].Key()
	})
	return out, nil
}

// ActionLinkPaths returns the link paths used by all expressions and templates of an action.
func (o *CompiledOntology) ActionLinkPaths(at *ActionType) ([]LinkPath, error) {
	var srcs []string
	for _, c := range at.SubmissionCriteria {
		srcs = append(srcs, c.Expr)
	}
	for _, r := range at.Rules {
		for _, k := range keys(r.Set) {
			srcs = append(srcs, r.Set[k])
		}
	}
	for _, se := range at.SideEffects {
		srcs = append(srcs, se.NotifyTo)
		if wh := o.Webhooks[se.Webhook]; wh != nil {
			srcs = append(srcs, wh.URL)
			srcs = append(srcs, bodyStrings(wh.Body)...)
			if wh.Compensate != nil {
				srcs = append(srcs, wh.Compensate.URL)
				srcs = append(srcs, bodyStrings(wh.Compensate.Body)...)
			}
		}
	}
	var out []LinkPath
	seen := map[string]bool{}
	for _, s := range srcs {
		paths, err := o.ResolveLinkPaths(at, s)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if !seen[p.Key()] {
				seen[p.Key()] = true
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Hops) != len(out[j].Hops) {
			return len(out[i].Hops) < len(out[j].Hops)
		}
		return out[i].Key() < out[j].Key()
	})
	return out, nil
}

var reAuthzTemplate = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// RenderTuple renders an authz tuple target template ("base:{{baseAirport}}") against object
// properties. ok is false when a referenced property is null or empty — no tuple is derived.
func RenderTuple(target string, props map[string]any) (string, bool) {
	ok := true
	out := reAuthzTemplate.ReplaceAllStringFunc(target, func(m string) string {
		name := reAuthzTemplate.FindStringSubmatch(m)[1]
		v, present := props[name]
		if !present || v == nil || fmt.Sprint(v) == "" {
			ok = false
			return ""
		}
		return fmt.Sprint(v)
	})
	return out, ok
}

// TupleProps lists the properties referenced by an object type's authz tuple templates.
func TupleProps(ot *ObjectType) []string {
	if ot.Authz == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range ot.Authz.Tuples {
		for _, m := range reAuthzTemplate.FindAllStringSubmatch(t.Target, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	sort.Strings(out)
	return out
}
