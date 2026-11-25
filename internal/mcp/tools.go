package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/acme/ontology/internal/spec"
)

// Read-only tool names.
const (
	ToolDescribe  = "ontology_describe"
	ToolGet       = "objects_get"
	ToolLinks     = "objects_links"
	ToolSearch    = "objects_search"
	ToolAggregate = "objects_aggregate"
	ToolValidate  = "action_validate"
	actionPrefix  = "action_"
)

func readOnly(title string) map[string]any {
	return map[string]any{"title": title, "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func typeEnum(ont *spec.CompiledOntology) []string {
	out := make([]string, 0, len(ont.ObjectTypes))
	for name := range ont.ObjectTypes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// whereSchema describes the filter DSL; nested conditions are left open (JSON Schema recursion
// support varies across clients), the backend validates them.
func whereSchema() map[string]any {
	cmp := map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}, "value": map[string]any{}},
		"required": []string{"field"}}
	return map[string]any{
		"type": "object",
		"description": "Filter. Exactly one operator per condition: and/or (arrays of conditions), not (condition), " +
			"eq/gt/gte/lt/lte/contains/startsWith ({field, value}), in ({field, values}), isNull ({field}), " +
			"link ({path: <link name>, where: <condition>}, at most 3 levels).",
		"properties": map[string]any{
			"and": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"or":  map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"not": map[string]any{"type": "object"},
			"eq":  cmp, "gt": cmp, "gte": cmp, "lt": cmp, "lte": cmp, "contains": cmp, "startsWith": cmp,
			"in": map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"},
				"values": map[string]any{"type": "array"}}, "required": []string{"field", "values"}},
			"isNull": map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}}, "required": []string{"field"}},
			"link": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"},
				"where": map[string]any{"type": "object"}}, "required": []string{"path"}},
		},
	}
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func readTools(ont *spec.CompiledOntology) []Tool {
	types := typeEnum(ont)
	objType := map[string]any{"type": "string", "enum": types, "description": "Object type apiName"}
	return []Tool{
		{Name: ToolDescribe, Title: "Describe the ontology",
			Description: "List object types with their properties and links, and the actions you may run. Call this first to learn names.",
			InputSchema: schema(map[string]any{"objectType": map[string]any{"type": "string", "enum": types,
				"description": "Limit the description to one object type"}}),
			Annotations: readOnly("Describe the ontology")},
		{Name: ToolGet, Title: "Get an object",
			Description: "Read one object by primary key. Properties above your clearance are null and listed in __redacted.",
			InputSchema: schema(map[string]any{"objectType": objType, "pk": map[string]any{"type": "string"}}, "objectType", "pk"),
			Annotations: readOnly("Get an object")},
		{Name: ToolLinks, Title: "Follow a link",
			Description: "List the objects linked to an object through a link name (see ontology_describe).",
			InputSchema: schema(map[string]any{"objectType": objType, "pk": map[string]any{"type": "string"},
				"link": map[string]any{"type": "string"}, "pageSize": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}},
				"objectType", "pk", "link"),
			Annotations: readOnly("Follow a link")},
		{Name: ToolSearch, Title: "Search objects",
			Description: "Find objects of one type with a filter, sort and pagination. Pass nextPageToken back as pageToken for more.",
			InputSchema: schema(map[string]any{"objectType": objType, "where": whereSchema(),
				"orderBy": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"field": map[string]any{"type": "string"}, "direction": map[string]any{"type": "string", "enum": []string{"asc", "desc"}}},
					"required": []string{"field"}}},
				"pageSize":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20},
				"pageToken": map[string]any{"type": "string"}}, "objectType"),
			Annotations: readOnly("Search objects")},
		{Name: ToolAggregate, Title: "Aggregate objects",
			Description: "Count, sum, avg, min or max over objects of one type, optionally grouped by properties.",
			InputSchema: schema(map[string]any{"objectType": objType, "where": whereSchema(),
				"groupBy": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"metrics": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"op":    map[string]any{"type": "string", "enum": []string{"count", "sum", "avg", "min", "max"}},
					"field": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}}, "required": []string{"op"}}}},
				"objectType"),
			Annotations: readOnly("Aggregate objects")},
	}
}

// exposedActions returns the actions marked ai.exposeToAgents, sorted by name.
func exposedActions(ont *spec.CompiledOntology) []*spec.ActionType {
	var out []*spec.ActionType
	for _, at := range ont.ActionTypes {
		if at.AI.ExposeToAgents {
			out = append(out, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].APIName < out[j].APIName })
	return out
}

func jsonType(b spec.BaseType) string {
	switch b {
	case spec.TInteger, spec.TLong:
		return "integer"
	case spec.TDouble:
		return "number"
	case spec.TBoolean:
		return "boolean"
	}
	return "string" // decimal (keeps precision), date, timestamp, string
}

func paramSchema(ont *spec.CompiledOntology, p *spec.ActionParam) map[string]any {
	var s map[string]any
	switch p.Type.Kind {
	case spec.KindObjectRef:
		s = map[string]any{"type": "string", "description": "Primary key of a " + p.Type.ObjectType}
	case spec.KindObjectRefList:
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": "Primary keys of " + p.Type.ObjectType + " objects"}
	default:
		s = map[string]any{"type": jsonType(p.Type.Base)}
		switch p.Type.Base {
		case spec.TTimestamp:
			s["format"] = "date-time"
		case spec.TDate:
			s["format"] = "date"
		case spec.TDecimal:
			s["description"] = "Decimal number as a string, e.g. \"12.50\""
		}
		c := p.Constraints
		if p.ValueType != "" {
			if vt := ont.ValueTypes[p.ValueType]; vt != nil && c.IsZero() {
				c = vt.Constraints
			}
		}
		if len(c.Enum) > 0 {
			s["enum"] = c.Enum
		}
		if c.Regex != nil {
			s["pattern"] = *c.Regex
		}
		if c.Min != nil {
			s["minimum"] = *c.Min
		}
		if c.Max != nil {
			s["maximum"] = *c.Max
		}
		if c.MinLen != nil {
			s["minLength"] = *c.MinLen
		}
		if c.MaxLen != nil {
			s["maxLength"] = *c.MaxLen
		}
	}
	if p.Default != nil {
		s["default"] = p.Default
	}
	return s
}

// actionTool turns an exposed action type into an MCP tool.
func actionTool(ont *spec.CompiledOntology, at *spec.ActionType) Tool {
	props := map[string]any{}
	var required []string
	for _, name := range at.ParamOrder {
		p := at.Parameters[name]
		props[name] = paramSchema(ont, p)
		if p.Required && p.Default == nil {
			required = append(required, name)
		}
	}
	desc := strings.TrimSpace(at.Description)
	if at.AI.AgentHint != "" {
		desc += "\n\nGuidance: " + at.AI.AgentHint
	}
	if at.AI.RequireHumanConfirmation {
		desc += "\n\nA human must confirm this action before it runs; the call returns a confirmation request first."
	}
	for _, c := range at.SubmissionCriteria {
		if c.Message != "" {
			desc += "\nPrecondition: " + c.Message
		}
	}
	title := at.DisplayName
	if title == "" {
		title = at.APIName
	}
	return Tool{
		Name:        actionPrefix + at.APIName,
		Title:       title,
		Description: desc,
		InputSchema: schema(props, required...),
		Annotations: map[string]any{"title": title, "readOnlyHint": false, "destructiveHint": hasDelete(at),
			"idempotentHint": false, "openWorldHint": len(at.SideEffects) > 0},
	}
}

func hasDelete(at *spec.ActionType) bool {
	for _, r := range at.Rules {
		if r.Kind == spec.RuleDelete {
			return true
		}
	}
	return at.Function != nil // a function's edits are not known in advance
}

func validateTool(actions []*spec.ActionType) Tool {
	names := make([]string, len(actions))
	for i, at := range actions {
		names[i] = at.APIName
	}
	return Tool{Name: ToolValidate, Title: "Validate an action",
		Description: "Check an action's parameters, permissions and preconditions without changing anything. Use before running an action.",
		InputSchema: schema(map[string]any{"action": map[string]any{"type": "string", "enum": names},
			"parameters": map[string]any{"type": "object"}}, "action", "parameters"),
		Annotations: readOnly("Validate an action")}
}

// BuildTools returns the tool list in deterministic order: read tools, then one tool per exposed
// action. action_validate is included only when at least one action is exposed.
func BuildTools(ont *spec.CompiledOntology) []Tool {
	tools := readTools(ont)
	actions := exposedActions(ont)
	if len(actions) > 0 {
		tools = append(tools, validateTool(actions))
	}
	for _, at := range actions {
		tools = append(tools, actionTool(ont, at))
	}
	return tools
}

// Describe renders the ontology for an agent: types, properties (with sensitivity), links and
// exposed actions. It contains schema only, never data.
func Describe(ont *spec.CompiledOntology, only string) (map[string]any, string) {
	var b strings.Builder
	types := []map[string]any{}
	for _, name := range typeEnum(ont) {
		if only != "" && name != only {
			continue
		}
		ot := ont.ObjectTypes[name]
		props := []map[string]any{}
		fmt.Fprintf(&b, "%s (%s), primary key %s\n", name, ot.DisplayName, ot.PrimaryKey)
		for _, pn := range ot.PropertyOrder {
			p := ot.Properties[pn]
			entry := map[string]any{"name": pn, "type": p.Type.String(), "required": p.Required, "sensitivity": string(p.Sensitivity)}
			if len(p.Constraints.Enum) > 0 {
				entry["enum"] = p.Constraints.Enum
			} else if vt := ont.ValueTypes[p.ValueType]; vt != nil && len(vt.Constraints.Enum) > 0 {
				entry["enum"] = vt.Constraints.Enum
			}
			props = append(props, entry)
			fmt.Fprintf(&b, "  - %s: %s", pn, p.Type.String())
			if e, ok := entry["enum"]; ok {
				fmt.Fprintf(&b, " %v", e)
			}
			b.WriteString("\n")
		}
		links := []map[string]any{}
		for _, nav := range ont.NavigationNames(name) {
			n, _ := ont.Navigate(name, nav)
			links = append(links, map[string]any{"name": nav, "target": n.Target, "many": n.Many})
			card := "one"
			if n.Many {
				card = "many"
			}
			fmt.Fprintf(&b, "  → %s: %s (%s)\n", nav, n.Target, card)
		}
		types = append(types, map[string]any{"name": name, "displayName": ot.DisplayName, "primaryKey": ot.PrimaryKey,
			"properties": props, "links": links})
	}
	actions := []map[string]any{}
	if only == "" {
		for _, at := range exposedActions(ont) {
			actions = append(actions, map[string]any{"tool": actionPrefix + at.APIName, "description": at.Description,
				"requiresConfirmation": at.AI.RequireHumanConfirmation})
			fmt.Fprintf(&b, "action %s%s: %s\n", actionPrefix, at.APIName, at.Description)
		}
	}
	return map[string]any{"ontology": ont.APIName, "version": ont.Version, "objectTypes": types, "actions": actions}, b.String()
}
