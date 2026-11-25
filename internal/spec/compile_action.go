package spec

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// BoolCompiler validates a CEL boolean expression for an action type.
// It is injected by the expr package (via SetBoolCompiler) to avoid an import cycle.
type BoolCompiler func(at *ActionType, expr string) error

// TemplateChecker validates every embedded expression in a template string.
type TemplateChecker func(at *ActionType, template string) error

var (
	boolCompiler    BoolCompiler
	templateChecker TemplateChecker
)

// SetExprHooks wires the CEL validators in. Called from an init in package expr's bridge file.
func SetExprHooks(bc BoolCompiler, tc TemplateChecker) {
	boolCompiler, templateChecker = bc, tc
}

type rawActionParam struct {
	Type        string      `yaml:"type"`
	Required    bool        `yaml:"required"`
	Default     any         `yaml:"default"`
	ValueType   string      `yaml:"valueType"`
	Constraints Constraints `yaml:"constraints"`
}

type rawRule struct {
	ModifyObject *struct {
		Object string            `yaml:"object"`
		Set    map[string]string `yaml:"set"`
	} `yaml:"modifyObject"`
	CreateObject *struct {
		ObjectType string            `yaml:"objectType"`
		Set        map[string]string `yaml:"set"`
	} `yaml:"createObject"`
	DeleteObject *struct {
		Object string `yaml:"object"`
	} `yaml:"deleteObject"`
	AddLink *struct {
		LinkType string `yaml:"linkType"`
		From     string `yaml:"from"`
		To       string `yaml:"to"`
	} `yaml:"addLink"`
	RemoveLink *struct {
		LinkType string `yaml:"linkType"`
		From     string `yaml:"from"`
		To       string `yaml:"to"`
	} `yaml:"removeLink"`
}

type rawActionType struct {
	APIName            string                     `yaml:"apiName"`
	DisplayName        string                     `yaml:"displayName"`
	Description        string                     `yaml:"description"`
	Parameters         map[string]*rawActionParam `yaml:"parameters"`
	SubmissionCriteria []Criterion                `yaml:"submissionCriteria"`
	Rules              []rawRule                  `yaml:"rules"`
	Function           *ActionFunction            `yaml:"function"`
	SideEffects        []map[string]any           `yaml:"sideEffects"`
	Permission         *ActionPermission          `yaml:"permission"`
	AI                 ActionAI                   `yaml:"ai"`
}

// ParamRef returns the parameter name in a bare "{{param}}" reference, or "" otherwise.
// Used at runtime by the action engine to resolve rule targets.
func ParamRef(s string) string { return paramRef(s) }

// paramRef extracts the single {{param}} identifier from s, or "" when s is not a bare param ref.
func paramRef(s string) string {
	m := reTemplate.FindStringSubmatch(s)
	if m == nil || reTemplate.FindString(s) != s {
		return ""
	}
	return m[1]
}

func (c *compiler) compileActionType(l located[rawActionType]) {
	r, f, n := l.v, l.file, l.node
	p := "actionTypes." + r.APIName
	c.checkKeys(f, n, p, "apiName", "displayName", "description", "parameters",
		"submissionCriteria", "rules", "function", "sideEffects", "permission", "ai")
	if !reMemberName.MatchString(r.APIName) {
		c.errf("G003", fieldPos(f, n, "apiName"), p+".apiName", "action type apiName %q must match %s", r.APIName, reMemberName)
	}
	if prev, ok := c.out.ActionTypes[r.APIName]; ok {
		c.errf("G002", posOf(f, n), p, "duplicate action type %q (first defined at %s)", r.APIName, prev.Pos)
		return
	}
	at := &ActionType{
		APIName: r.APIName, DisplayName: r.DisplayName, Description: r.Description,
		Parameters: map[string]*ActionParam{}, SubmissionCriteria: r.SubmissionCriteria,
		Function: r.Function, Permission: r.Permission, AI: r.AI, Pos: posOf(f, n),
	}
	if at.DisplayName == "" {
		at.DisplayName = r.APIName
	}
	c.out.ActionTypes[r.APIName] = at
	c.compileActionParams(f, n, p, at, r.Parameters)
	c.compileActionRules(f, n, p, at, r.Rules)
	c.compileSideEffects(f, n, p, at, r.SideEffects)

	// AT001: exactly one of rules / function.
	hasRules, hasFn := len(at.Rules) > 0, at.Function != nil
	if hasRules == hasFn {
		c.errf("AT001", posOf(f, n), p, "an action type must have either rules or a function, but not both (and not neither)")
	}
	if hasFn && at.Function.Runtime != "typescript" && at.Function.Runtime != "python" && at.Function.Runtime != "go" {
		c.errf("AT001", fieldPos(f, n, "function"), p+".function.runtime", "function runtime must be typescript, python or go; got %q", at.Function.Runtime)
	}

	// AT003: submission criteria must compile to bool.
	if boolCompiler != nil {
		for i, cr := range at.SubmissionCriteria {
			if err := boolCompiler(at, cr.Expr); err != nil {
				c.errf("AT003", posOf(f, n), fmt.Sprintf("%s.submissionCriteria[%d]", p, i), "invalid expression: %v", err)
			}
		}
	}

	// AT011: link traversal in expressions and templates must follow existing single-valued links.
	if _, err := c.out.ActionLinkPaths(at); err != nil {
		c.errf("AT011", posOf(f, n), p, "invalid link traversal: %v", err)
	}

	// AT009: permission.on references an objectRef parameter whose object type has authz.
	if at.Permission != nil {
		ref := paramRef(at.Permission.On)
		pp, ok := at.Parameters[ref]
		switch {
		case ref == "" || !ok:
			c.errf("AT009", fieldPos(f, n, "permission"), p+".permission.on", "permission.on must reference an objectRef parameter as {{param}}; got %q", at.Permission.On)
		case pp.Type.Kind != KindObjectRef:
			c.errf("AT009", fieldPos(f, n, "permission"), p+".permission.on", "permission.on parameter %q must be an objectRef", ref)
		default:
			if ot, ok := c.out.ObjectTypes[pp.Type.ObjectType]; ok && ot.Authz == nil {
				c.errf("AT009", fieldPos(f, n, "permission"), p+".permission.on", "object type %q has no authz block, but is used for permission.on", pp.Type.ObjectType)
			}
		}
	}

	if at.AI.ExposeToAgents && at.Description == "" {
		c.warnf("AT010", posOf(f, n), p, "action %q is exposed to agents but has no description", at.APIName)
	}
}

func (c *compiler) compileActionParams(f string, n *yaml.Node, p string, at *ActionType, raw map[string]*rawActionParam) {
	pn := fieldNode(n, "parameters")
	for _, e := range mapEntries(pn) {
		name := e.key.Value
		pp := p + ".parameters." + name
		if !reMemberName.MatchString(name) {
			c.errf("G003", posOf(f, e.key), pp, "parameter name %q must match %s", name, reMemberName)
			continue
		}
		rp := raw[name]
		if rp == nil {
			c.errf("AT002", posOf(f, e.val), pp, "parameter %q must be a mapping with a type", name)
			continue
		}
		ap := &ActionParam{APIName: name, Required: rp.Required, Default: rp.Default,
			ValueType: rp.ValueType, Constraints: rp.Constraints, Pos: posOf(f, e.key)}
		ap.Type = c.parseParamType(f, e.val, pp, rp.Type)
		if ap.Type.Kind == KindPrimitive && ap.ValueType != "" {
			if vt, ok := c.out.ValueTypes[ap.ValueType]; !ok {
				c.errf("AT002", posOf(f, e.val), pp, "valueType %q not found%s", ap.ValueType, didYouMean(ap.ValueType, keys(c.out.ValueTypes)))
			} else if vt.BaseType != ap.Type.Base {
				c.errf("AT002", posOf(f, e.val), pp, "valueType %q baseType %s != parameter type %s", ap.ValueType, vt.BaseType, ap.Type.Base)
			}
		}
		at.Parameters[name] = ap
		at.ParamOrder = append(at.ParamOrder, name)
	}
}

var reObjectRefList = regexp.MustCompile(`^objectRefList<([A-Za-z0-9]+)>$`)
var reObjectRef = regexp.MustCompile(`^objectRef<([A-Za-z0-9]+)>$`)

func (c *compiler) parseParamType(f string, n *yaml.Node, p, s string) ParamType {
	if m := reObjectRef.FindStringSubmatch(s); m != nil {
		if _, ok := c.out.ObjectTypes[m[1]]; !ok {
			c.errf("AT002", posOf(f, n), p, "objectRef references unknown object type %q%s", m[1], didYouMean(m[1], keys(c.out.ObjectTypes)))
		}
		return ParamType{Kind: KindObjectRef, ObjectType: m[1]}
	}
	if m := reObjectRefList.FindStringSubmatch(s); m != nil {
		if _, ok := c.out.ObjectTypes[m[1]]; !ok {
			c.errf("AT002", posOf(f, n), p, "objectRefList references unknown object type %q", m[1])
		}
		return ParamType{Kind: KindObjectRefList, ObjectType: m[1]}
	}
	tr, err := ParseTypeRef(s)
	if err != nil || !tr.Base.IsScalar() {
		c.errf("AT002", posOf(f, n), p, "parameter type must be a scalar, objectRef<T> or objectRefList<T>; got %q", s)
		return ParamType{Kind: KindPrimitive, Base: TString}
	}
	return ParamType{Kind: KindPrimitive, Base: tr.Base}
}

func (c *compiler) compileActionRules(f string, n *yaml.Node, p string, at *ActionType, raws []rawRule) {
	checkObjParam := func(ppath, ref string) *ActionParam {
		name := paramRef(ref)
		ap, ok := at.Parameters[name]
		if name == "" || !ok {
			c.errf("AT005", posOf(f, n), ppath, "rule object must reference an objectRef parameter as {{param}}; got %q", ref)
			return nil
		}
		if ap.Type.Kind != KindObjectRef {
			c.errf("AT005", posOf(f, n), ppath, "rule parameter %q must be an objectRef", name)
			return nil
		}
		return ap
	}
	checkSet := func(ppath, objType string, set map[string]string, forCreate bool) {
		ot, ok := c.out.ObjectTypes[objType]
		if !ok {
			return
		}
		assigned := map[string]bool{}
		for _, prop := range keys(set) {
			if _, ok := ot.Properties[prop]; !ok {
				c.errf("AT004", posOf(f, n), ppath+"."+prop, "property %q not found on %s%s", prop, objType, didYouMean(prop, ot.PropertyOrder))
				continue
			}
			assigned[prop] = true
			if templateChecker != nil {
				if err := templateChecker(at, set[prop]); err != nil {
					c.errf("AT004", posOf(f, n), ppath+"."+prop, "invalid value template: %v", err)
				}
			}
		}
		if forCreate {
			for _, name := range ot.PropertyOrder {
				pr := ot.Properties[name]
				if (pr.Required || name == ot.PrimaryKey) && !assigned[name] {
					c.errf("AT006", posOf(f, n), ppath, "createObject must set required property %q", name)
				}
			}
		}
	}

	for i, raw := range raws {
		rp := fmt.Sprintf("%s.rules[%d]", p, i)
		set := 0
		if raw.ModifyObject != nil {
			set++
			if ap := checkObjParam(rp+".modifyObject.object", raw.ModifyObject.Object); ap != nil {
				checkSet(rp+".modifyObject.set", ap.Type.ObjectType, raw.ModifyObject.Set, false)
			}
			at.Rules = append(at.Rules, Rule{Kind: RuleModify, Object: raw.ModifyObject.Object, Set: raw.ModifyObject.Set, Pos: posOf(f, n)})
		}
		if raw.CreateObject != nil {
			set++
			checkSet(rp+".createObject.set", raw.CreateObject.ObjectType, raw.CreateObject.Set, true)
			if _, ok := c.out.ObjectTypes[raw.CreateObject.ObjectType]; !ok {
				c.errf("AT004", posOf(f, n), rp+".createObject.objectType", "unknown object type %q", raw.CreateObject.ObjectType)
			}
			at.Rules = append(at.Rules, Rule{Kind: RuleCreate, ObjectType: raw.CreateObject.ObjectType, Set: raw.CreateObject.Set, Pos: posOf(f, n)})
		}
		if raw.DeleteObject != nil {
			set++
			checkObjParam(rp+".deleteObject.object", raw.DeleteObject.Object)
			at.Rules = append(at.Rules, Rule{Kind: RuleDelete, Object: raw.DeleteObject.Object, Pos: posOf(f, n)})
		}
		if raw.AddLink != nil {
			set++
			c.checkLinkRule(f, n, rp+".addLink", at, raw.AddLink.LinkType)
			at.Rules = append(at.Rules, Rule{Kind: RuleLink, LinkType: raw.AddLink.LinkType, From: raw.AddLink.From, To: raw.AddLink.To, Pos: posOf(f, n)})
		}
		if raw.RemoveLink != nil {
			set++
			c.checkLinkRule(f, n, rp+".removeLink", at, raw.RemoveLink.LinkType)
			at.Rules = append(at.Rules, Rule{Kind: RuleUnlink, LinkType: raw.RemoveLink.LinkType, From: raw.RemoveLink.From, To: raw.RemoveLink.To, Pos: posOf(f, n)})
		}
		if set != 1 {
			c.errf("AT004", posOf(f, n), rp, "each rule must contain exactly one of modifyObject, createObject, deleteObject, addLink, removeLink")
		}
	}
}

func (c *compiler) checkLinkRule(f string, n *yaml.Node, p string, at *ActionType, linkType string) {
	if _, ok := c.out.LinkTypes[linkType]; !ok {
		c.errf("AT004", posOf(f, n), p+".linkType", "unknown link type %q%s", linkType, didYouMean(linkType, keys(c.out.LinkTypes)))
	}
}

func (c *compiler) compileSideEffects(f string, n *yaml.Node, p string, at *ActionType, raws []map[string]any) {
	writeback := 0
	for i, raw := range raws {
		sp := fmt.Sprintf("%s.sideEffects[%d]", p, i)
		se := SideEffect{Pos: posOf(f, n)}
		if wh, ok := raw["webhook"]; ok {
			name, _ := wh.(string)
			se.Webhook = name
			mode := WebhookSideEffect
			if m, ok := raw["mode"].(string); ok && m != "" {
				mode = WebhookMode(m)
			}
			se.WebhookMode = mode
			if mode != WebhookWriteback && mode != WebhookSideEffect {
				c.errf("AT004", posOf(f, n), sp+".mode", "webhook mode must be WRITEBACK or SIDE_EFFECT; got %q", mode)
			}
			if mode == WebhookWriteback {
				writeback++
			}
			if _, ok := c.out.Webhooks[name]; !ok {
				c.errf("AT007", posOf(f, n), sp+".webhook", "webhook %q is not defined%s", name, didYouMean(name, keys(c.out.Webhooks)))
			}
		} else if to, ok := raw["notifyTo"]; ok {
			se.NotifyTo, _ = to.(string)
			if tpl, ok := raw["template"].(string); ok {
				se.Template = tpl
			}
		} else {
			c.errf("AT004", posOf(f, n), sp, "side effect must have either a webhook or notifyTo")
		}
		at.SideEffects = append(at.SideEffects, se)
	}
	if writeback > 1 {
		c.errf("AT008", posOf(f, n), p+".sideEffects", "an action type may have at most one WRITEBACK webhook, found %d", writeback)
	}
}

// ---------- webhooks ----------

func (c *compiler) compileWebhook(l located[Webhook]) {
	w, f, n := l.v, l.file, l.node
	p := "webhooks." + w.Name
	if w.Name == "" {
		c.errf("G005", posOf(f, n), p, "webhook name is required")
		return
	}
	if prev, ok := c.out.Webhooks[w.Name]; ok {
		c.errf("G002", posOf(f, n), p, "duplicate webhook %q (first defined at %s)", w.Name, prev.Pos)
		return
	}
	if w.TimeoutMs == 0 {
		w.TimeoutMs = 5000
	}
	if w.Retry.Max == 0 {
		w.Retry = WebhookRetry{Max: 3, Backoff: "exponential"}
	}
	w.Pos = posOf(f, n)
	wp := &w
	c.out.Webhooks[w.Name] = wp

	// WH002: secretRef must use vault:// or env://
	if w.Auth.Type != "" && w.Auth.Type != "none" {
		if sr := w.Auth.SecretRef; len(sr) < 8 || (sr[:8] != "vault://" && sr[:6] != "env://") {
			c.errf("WH002", posOf(f, n), p+".auth.secretRef", "auth.secretRef must start with vault:// or env://; got %q", sr)
		}
	}
	// WH001: URL/body templates may only reference parameters of actions, which we cannot
	// bind to a single action here; we instead validate that templates are syntactically closed.
	for _, s := range append([]string{w.URL}, bodyStrings(w.Body)...) {
		if openN, closeN := countTemplates(s); openN != closeN {
			c.errf("WH001", posOf(f, n), p, "unbalanced template braces in %q", s)
		}
	}
}

func bodyStrings(m map[string]any) []string {
	var out []string
	for _, k := range keysAny(m) {
		if s, ok := m[k].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func keysAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func countTemplates(s string) (open, close int) {
	return countSub(s, "{{"), countSub(s, "}}")
}

func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
			i += len(sub) - 1
		}
	}
	return n
}

// ---------- migrations ----------

func (c *compiler) compileMigration(l located[Migration]) {
	m := l.v
	m.Pos = posOf(l.file, l.node)
	c.out.Migrations = append(c.out.Migrations, m)
}
