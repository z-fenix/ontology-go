package spec

import (
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "strings"

    "gopkg.in/yaml.v3"
)

// MaxPropertiesPerObjectType is the MVP limit (OT004).
const MaxPropertiesPerObjectType = 500

var knownKinds = []string{"ontology", "valueTypes", "objectType", "objectTypes", "linkTypes",
    "actionType", "actionTypes", "webhooks", "migrations"}

type located[T any] struct {
    v    T
    node *yaml.Node
    file string
}

type rawOntology struct {
    APIName     string               `yaml:"apiName"`
    DisplayName string               `yaml:"displayName"`
    Connectors  map[string]Connector `yaml:"connectors"`
}

type rawValueType struct {
    APIName     string      `yaml:"apiName"`
    BaseType    string      `yaml:"baseType"`
    Constraints Constraints `yaml:"constraints"`
}

type rawProperty struct {
    Type        string      `yaml:"type"`
    Required    bool        `yaml:"required"`
    ValueType   string      `yaml:"valueType"`
    Constraints Constraints `yaml:"constraints"`
    Sensitivity string      `yaml:"sensitivity"`
    DerivedFrom string      `yaml:"derivedFrom"`
}

type rawDatasource struct {
    Connector        string            `yaml:"connector"`
    Table            string            `yaml:"table"`
    ColumnMapping    map[string]string `yaml:"columnMapping"`
    ConflictStrategy string            `yaml:"conflictStrategy"`
    TimestampColumn  string            `yaml:"timestampColumn"`
}

// properties are read directly from the YAML node to keep per-key positions.
type rawObjectType struct {
    APIName       string         `yaml:"apiName"`
    DisplayName   string         `yaml:"displayName"`
    PrimaryKey    string         `yaml:"primaryKey"`
    TitleProperty string         `yaml:"titleProperty"`
    Datasource    *rawDatasource `yaml:"datasource"`
    Authz         *ObjectAuthz   `yaml:"authz"`
}

type rawLinkSide struct {
    ObjectType  string `yaml:"objectType"`
    APIName     string `yaml:"apiName"`
    Cardinality string `yaml:"cardinality"`
}

type rawLinkType struct {
    APIName    string      `yaml:"apiName"`
    From       rawLinkSide `yaml:"from"`
    To         rawLinkSide `yaml:"to"`
    ForeignKey *ForeignKey `yaml:"foreignKey"`
    JoinTable  *JoinTable  `yaml:"joinTable"`
}

type compiler struct {
    diags       Diagnostics
    ontologies  []located[rawOntology]
    valueTypes  []located[rawValueType]
    objectTypes []located[rawObjectType]
    linkTypes   []located[rawLinkType]
    actionTypes []located[rawActionType]
    webhooks    []located[Webhook]
    migrations  []located[Migration]
    linkNames   map[string]map[string]string // objectType -> navigation name -> link apiName
    out         *CompiledOntology
}

// Compile turns loaded files into a CompiledOntology, collecting all diagnostics.
// The returned ontology may be partial when diagnostics contain errors.
func Compile(files []*File) (*CompiledOntology, Diagnostics) {
    c := &compiler{
        linkNames: map[string]map[string]string{},
        out: &CompiledOntology{
            Connectors:  map[string]Connector{},
            ValueTypes:  map[string]*ValueType{},
            ObjectTypes: map[string]*ObjectType{},
            LinkTypes:   map[string]*LinkType{},
            ActionTypes: map[string]*ActionType{},
            Webhooks:    map[string]*Webhook{},
        },
    }
    for _, f := range files {
        c.collect(f)
    }
    c.compileOntology()
    for _, l := range c.valueTypes {
        c.compileValueType(l)
    }
    for _, l := range c.objectTypes {
        c.compileObjectType(l)
    }
    for _, l := range c.linkTypes {
        c.compileLinkType(l)
    }
    for _, l := range c.webhooks {
        c.compileWebhook(l)
    }
    for _, l := range c.actionTypes {
        c.compileActionType(l)
    }
    for _, l := range c.migrations {
        c.compileMigration(l)
    }
    if !c.diags.HasErrors(false) {
        sum, err := checksum(c.out)
        if err != nil {
            c.errf("G000", SourcePos{}, "", "checksum: %v", err)
        } else {
            c.out.Checksum = sum
        }
    }
    c.diags.Sort()
    return c.out, c.diags
}

// ---------- diagnostics helpers ----------

func (c *compiler) errf(code string, pos SourcePos, p, format string, args ...any) {
    c.diags = append(c.diags, Diagnostic{Code: code, Severity: SevError, Message: fmt.Sprintf(format, args...), Path: p, Pos: pos})
}

func (c *compiler) warnf(code string, pos SourcePos, p, format string, args ...any) {
    c.diags = append(c.diags, Diagnostic{Code: code, Severity: SevWarning, Message: fmt.Sprintf(format, args...), Path: p, Pos: pos})
}

func (c *compiler) decode(file string, n *yaml.Node, out any, p string) bool {
    if err := n.Decode(out); err != nil {
        c.errf("G005", posOf(file, n), p, "invalid structure: %v", err)
        return false
    }
    return true
}

// checkKeys reports unknown fields (G005) so typos are never silently ignored.
func (c *compiler) checkKeys(file string, n *yaml.Node, p string, allowed ...string) {
    set := make(map[string]bool, len(allowed))
    for _, a := range allowed {
        set[a] = true
    }
    for _, e := range mapEntries(n) {
        if !set[e.key.Value] {
            c.errf("G005", posOf(file, e.key), p, "unknown field %q%s", e.key.Value, didYouMean(e.key.Value, allowed))
        }
    }
}

// ---------- collection ----------

func (c *compiler) collect(f *File) {
    root := f.Root
    if root.Kind != yaml.MappingNode || len(root.Content) != 2 {
        c.errf("G006", posOf(f.Path, root), "", "a file must contain exactly one top-level kind key (one of %s)", strings.Join(knownKinds, ", "))
        return
    }
    key, val := root.Content[0], root.Content[1]
    switch key.Value {
    case "ontology":
        var r rawOntology
        if c.decode(f.Path, val, &r, "ontology") {
            c.ontologies = append(c.ontologies, located[rawOntology]{r, val, f.Path})
        }
    case "valueTypes":
        c.eachItem(f, val, "valueTypes", func(n *yaml.Node) {
            var r rawValueType
            if c.decode(f.Path, n, &r, "valueTypes") {
                c.valueTypes = append(c.valueTypes, located[rawValueType]{r, n, f.Path})
            }
        })
    case "objectType":
        c.addObjectType(f, val)
    case "objectTypes":
        c.eachItem(f, val, "objectTypes", func(n *yaml.Node) { c.addObjectType(f, n) })
    case "linkTypes":
        c.eachItem(f, val, "linkTypes", func(n *yaml.Node) {
            var r rawLinkType
            if c.decode(f.Path, n, &r, "linkTypes") {
                c.linkTypes = append(c.linkTypes, located[rawLinkType]{r, n, f.Path})
            }
        })
    case "actionType":
        var r rawActionType
        if c.decode(f.Path, val, &r, "actionType") {
            c.actionTypes = append(c.actionTypes, located[rawActionType]{r, val, f.Path})
        }
    case "actionTypes":
        c.eachItem(f, val, "actionTypes", func(n *yaml.Node) {
            var r rawActionType
            if c.decode(f.Path, n, &r, "actionTypes") {
                c.actionTypes = append(c.actionTypes, located[rawActionType]{r, n, f.Path})
            }
        })
    case "webhooks":
        c.eachItem(f, val, "webhooks", func(n *yaml.Node) {
            var w Webhook
            if c.decode(f.Path, n, &w, "webhooks") {
                c.webhooks = append(c.webhooks, located[Webhook]{w, n, f.Path})
            }
        })
    case "migrations":
        c.eachItem(f, val, "migrations", func(n *yaml.Node) {
            var m Migration
            if c.decode(f.Path, n, &m, "migrations") {
                c.migrations = append(c.migrations, located[Migration]{m, n, f.Path})
            }
        })
    default:
        c.errf("G007", posOf(f.Path, key), key.Value, "unknown top-level kind %q%s", key.Value, didYouMean(key.Value, knownKinds))
    }
}

func (c *compiler) addObjectType(f *File, n *yaml.Node) {
    var r rawObjectType
    if c.decode(f.Path, n, &r, "objectType") {
        c.objectTypes = append(c.objectTypes, located[rawObjectType]{r, n, f.Path})
    }
}

func (c *compiler) eachItem(f *File, n *yaml.Node, p string, fn func(*yaml.Node)) {
    if n.Kind != yaml.SequenceNode {
        c.errf("G005", posOf(f.Path, n), p, "%s must be a list", p)
        return
    }
    for _, item := range n.Content {
        fn(item)
    }
}

// ---------- ontology ----------

func (c *compiler) compileOntology() {
    if len(c.ontologies) == 0 {
        c.errf("G001", SourcePos{}, "ontology", "exactly one `ontology` definition is required, found none")
        return
    }
    first := c.ontologies[0]
    for _, o := range c.ontologies[1:] {
        c.errf("G001", posOf(o.file, o.node), "ontology", "duplicate `ontology` definition (first defined at %s)", posOf(first.file, first.node))
    }
    c.checkKeys(first.file, first.node, "ontology", "apiName", "displayName", "connectors")
    if !reOntologyName.MatchString(first.v.APIName) {
        c.errf("G003", fieldPos(first.file, first.node, "apiName"), "ontology.apiName", "ontology apiName %q must match %s", first.v.APIName, reOntologyName)
    }
    c.out.APIName = first.v.APIName
    c.out.DisplayName = first.v.DisplayName
    for k, v := range first.v.Connectors {
        c.out.Connectors[k] = v
    }
}

// ---------- value types ----------

func (c *compiler) compileValueType(l located[rawValueType]) {
    r, f, n := l.v, l.file, l.node
    p := "valueTypes." + r.APIName
    c.checkKeys(f, n, p, "apiName", "baseType", "constraints")
    if !reTypeName.MatchString(r.APIName) {
        c.errf("G003", fieldPos(f, n, "apiName"), p+".apiName", "value type apiName %q must match %s", r.APIName, reTypeName)
    }
    if prev, ok := c.out.ValueTypes[r.APIName]; ok {
        c.errf("G002", posOf(f, n), p, "duplicate value type %q (first defined at %s)", r.APIName, prev.Pos)
        return
    }
    base := BaseType(r.BaseType)
    if !base.IsScalar() {
        c.errf("VT001", fieldPos(f, n, "baseType"), p+".baseType", "baseType must be a scalar type (string, integer, long, double, decimal, boolean, date, timestamp); got %q", r.BaseType)
    } else {
        for _, is := range checkConstraints(base, r.Constraints) {
            c.errf(is.code, fieldPos(f, n, "constraints"), p+".constraints", "%s", is.msg)
        }
    }
    c.out.ValueTypes[r.APIName] = &ValueType{APIName: r.APIName, BaseType: base, Constraints: r.Constraints, Pos: posOf(f, n)}
}

// ---------- object types ----------

func (c *compiler) compileObjectType(l located[rawObjectType]) {
    r, f, n := l.v, l.file, l.node
    p := "objectTypes." + r.APIName
    c.checkKeys(f, n, p, "apiName", "displayName", "primaryKey", "titleProperty", "properties", "datasource", "authz")
    if !reTypeName.MatchString(r.APIName) {
        c.errf("G003", fieldPos(f, n, "apiName"), p+".apiName", "object type apiName %q must match %s", r.APIName, reTypeName)
    }
    if prev, ok := c.out.ObjectTypes[r.APIName]; ok {
        c.errf("G002", posOf(f, n), p, "duplicate object type %q (first defined at %s)", r.APIName, prev.Pos)
        return
    }
    ot := &ObjectType{
        APIName:       r.APIName,
        DisplayName:   r.DisplayName,
        PrimaryKey:    r.PrimaryKey,
        TitleProperty: r.TitleProperty,
        Properties:    map[string]*Property{},
        Authz:         r.Authz,
        TableName:     "obj." + ToSnake(r.APIName),
        Pos:           posOf(f, n),
    }
    if ot.DisplayName == "" {
        ot.DisplayName = r.APIName
    }
    c.out.ObjectTypes[r.APIName] = ot

    c.compileProperties(f, n, p, ot)
    c.compileDatasource(f, n, p, ot, r.Datasource)
    c.assignColumns(p, ot)
    c.checkPrimaryKeyAndTitle(f, n, p, ot)
    c.checkAuthz(f, n, p, ot)
}

func (c *compiler) compileProperties(f string, n *yaml.Node, p string, ot *ObjectType) {
    propsNode := fieldNode(n, "properties")
    if propsNode == nil || propsNode.Kind != yaml.MappingNode || len(propsNode.Content) == 0 {
        c.errf("OT013", fieldPos(f, n, "properties"), p+".properties", "properties must be a non-empty mapping of propertyName -> definition")
        return
    }
    entries := mapEntries(propsNode)
    if len(entries) > MaxPropertiesPerObjectType {
        c.errf("OT004", posOf(f, propsNode), p+".properties", "object type has %d properties; the limit is %d", len(entries), MaxPropertiesPerObjectType)
    }
    for _, e := range entries {
        name := e.key.Value
        pp := p + ".properties." + name
        if strings.HasPrefix(name, "_") {
            c.errf("G004", posOf(f, e.key), pp, "property name %q is reserved: names starting with '_' are system columns", name)
            continue
        }
        if !reMemberName.MatchString(name) {
            c.errf("G003", posOf(f, e.key), pp, "property name %q must match %s", name, reMemberName)
        }
        if _, dup := ot.Properties[name]; dup {
            c.errf("G002", posOf(f, e.key), pp, "duplicate property %q", name)
            continue
        }
        var rp rawProperty
        if e.val.Kind == yaml.ScalarNode { // shorthand: `name: string`
            rp.Type = e.val.Value
        } else {
            c.checkKeys(f, e.val, pp, "type", "required", "valueType", "constraints", "sensitivity", "derivedFrom")
            if !c.decode(f, e.val, &rp, pp) {
                continue
            }
        }
        prop := &Property{
            APIName:     name,
            Required:    rp.Required,
            ValueType:   rp.ValueType,
            Constraints: rp.Constraints,
            Sensitivity: Sensitivity(rp.Sensitivity),
            DerivedFrom: rp.DerivedFrom,
            Pos:         posOf(f, e.key),
        }
        if prop.Sensitivity == "" {
            prop.Sensitivity = SensInternal
        } else if !prop.Sensitivity.Valid() {
            c.errf("OT012", fieldPos(f, e.val, "sensitivity"), pp+".sensitivity", "sensitivity must be one of PUBLIC, INTERNAL, CONFIDENTIAL, RESTRICTED; got %q", prop.Sensitivity)
        }
        tr, err := ParseTypeRef(rp.Type)
        if err != nil {
            c.errf("OT011", fieldPos(f, e.val, "type"), pp+".type", "invalid property type: %v", err)
        } else {
            prop.Type = tr
            if prop.ValueType != "" {
                vt, ok := c.out.ValueTypes[prop.ValueType]
                switch {
                case !ok:
                    c.errf("OT003", fieldPos(f, e.val, "valueType"), pp, "valueType %q not found%s", prop.ValueType, didYouMean(prop.ValueType, keys(c.out.ValueTypes)))
                case vt.BaseType != tr.Base:
                    c.errf("OT003", fieldPos(f, e.val, "valueType"), pp, "valueType %q has baseType %s but the property type is %s", prop.ValueType, vt.BaseType, tr)
                }
            }
            for _, is := range checkConstraints(tr.Base, rp.Constraints) {
                c.errf(is.code, fieldPos(f, e.val, "constraints"), pp+".constraints", "%s", is.msg)
            }
        }
        ot.Properties[name] = prop
        ot.PropertyOrder = append(ot.PropertyOrder, name)
    }

    for _, name := range ot.PropertyOrder {
        prop := ot.Properties[name]
        if prop.DerivedFrom == "" {
            continue
        }
        src, ok := ot.Properties[prop.DerivedFrom]
        if prop.Type.Base != TVector || !ok || src.Type.Base != TString {
            c.errf("OT008", prop.Pos, p+".properties."+name+".derivedFrom",
                "derivedFrom requires a vector property referencing a string property of the same object type; got %q%s",
                prop.DerivedFrom, didYouMean(prop.DerivedFrom, ot.PropertyOrder))
        }
    }
}

func (c *compiler) compileDatasource(f string, n *yaml.Node, p string, ot *ObjectType, rd *rawDatasource) {
    if rd == nil {
        c.warnf("OT009", posOf(f, n), p, "object type %q has no datasource; it will only contain objects created by actions", ot.APIName)
        return
    }
    dn := fieldNode(n, "datasource")
    dp := p + ".datasource"
    c.checkKeys(f, dn, dp, "connector", "table", "columnMapping", "conflictStrategy", "timestampColumn")
    ds := &Datasource{
        Connector:        rd.Connector,
        Table:            rd.Table,
        ColumnMapping:    rd.ColumnMapping,
        ConflictStrategy: ConflictStrategy(rd.ConflictStrategy),
        TimestampColumn:  rd.TimestampColumn,
    }
    if ds.ConflictStrategy == "" {
        ds.ConflictStrategy = ApplyUserEdits
    }
    if ds.ConflictStrategy != ApplyUserEdits && ds.ConflictStrategy != MostRecentValue {
        c.errf("G005", fieldPos(f, dn, "conflictStrategy"), dp+".conflictStrategy", "conflictStrategy must be APPLY_USER_EDITS or MOST_RECENT_VALUE; got %q", ds.ConflictStrategy)
    }
    if _, ok := c.out.Connectors[ds.Connector]; !ok {
        c.errf("OT005", fieldPos(f, dn, "connector"), dp+".connector", "connector %q is not declared in ontology.connectors%s", ds.Connector, didYouMean(ds.Connector, keys(c.out.Connectors)))
    }
    if ds.Table == "" {
        c.errf("G005", posOf(f, dn), dp+".table", "datasource.table is required")
    }
    if ds.ConflictStrategy == MostRecentValue && ds.TimestampColumn == "" {
        c.errf("OT006", fieldPos(f, dn, "conflictStrategy"), dp+".timestampColumn", "timestampColumn is required when conflictStrategy is MOST_RECENT_VALUE")
    }
    for _, prop := range keys(ds.ColumnMapping) {
        if _, ok := ot.Properties[prop]; !ok {
            c.errf("OT015", fieldPos(f, dn, "columnMapping"), dp+".columnMapping."+prop, "columnMapping references unknown property %q%s", prop, didYouMean(prop, ot.PropertyOrder))
        }
    }
    ot.Datasource = ds
}

func (c *compiler) assignColumns(p string, ot *ObjectType) {
    owner := map[string]string{}
    for _, name := range ot.PropertyOrder {
        prop := ot.Properties[name]
        col := ToSnake(name)
        if ot.Datasource != nil {
            if m := ot.Datasource.ColumnMapping[name]; m != "" {
                col = m
            }
        }
        prop.Column = col
        if other, ok := owner[col]; ok {
            c.errf("OT007", prop.Pos, p+".properties."+name, "properties %q and %q both map to column %q", other, name, col)
            continue
        }
        owner[col] = name
    }
}

func (c *compiler) checkPrimaryKeyAndTitle(f string, n *yaml.Node, p string, ot *ObjectType) {
    pkPos := fieldPos(f, n, "primaryKey")
    pk, ok := ot.Properties[ot.PrimaryKey]
    switch {
    case ot.PrimaryKey == "":
        c.errf("OT001", pkPos, p+".primaryKey", "primaryKey is required")
    case !ok:
        c.errf("OT001", pkPos, p+".primaryKey", "primaryKey %q is not a declared property%s", ot.PrimaryKey, didYouMean(ot.PrimaryKey, ot.PropertyOrder))
    case pk.Type.Base != TString && pk.Type.Base != TInteger && pk.Type.Base != TLong:
        c.errf("OT001", pkPos, p+".primaryKey", "primary key property %q must be string, integer or long; got %s", ot.PrimaryKey, pk.Type)
    case !pk.Required:
        c.errf("OT001", pkPos, p+".primaryKey", "primary key property %q must be declared with required: true", ot.PrimaryKey)
    }

    tPos := fieldPos(f, n, "titleProperty")
    if ot.TitleProperty == "" {
        c.errf("OT002", tPos, p+".titleProperty", "titleProperty is required")
    } else if _, ok := ot.Properties[ot.TitleProperty]; !ok {
        c.errf("OT002", tPos, p+".titleProperty", "titleProperty %q is not a declared property%s", ot.TitleProperty, didYouMean(ot.TitleProperty, ot.PropertyOrder))
    }
}

func (c *compiler) checkAuthz(f string, n *yaml.Node, p string, ot *ObjectType) {
    if ot.Authz == nil {
        return
    }
    an := fieldNode(n, "authz")
    ap := p + ".authz"
    c.checkKeys(f, an, ap, "fgaType", "tuples")
    if ot.Authz.FGAType == "" {
        c.errf("G005", posOf(f, an), ap+".fgaType", "authz.fgaType is required")
    }
    for i, t := range ot.Authz.Tuples {
        tp := fmt.Sprintf("%s.tuples[%d]", ap, i)
        if t.Relation == "" || t.Target == "" {
            c.errf("G005", posOf(f, an), tp, "authz tuple requires relation and target")
        }
        for _, m := range reTemplate.FindAllStringSubmatch(t.Target, -1) {
            if _, ok := ot.Properties[m[1]]; !ok {
                c.errf("OT010", posOf(f, an), tp+".target", "template references unknown property %q%s", m[1], didYouMean(m[1], ot.PropertyOrder))
            }
        }
    }
}

// ---------- link types ----------

func (c *compiler) compileLinkType(l located[rawLinkType]) {
    r, f, n := l.v, l.file, l.node
    p := "linkTypes." + r.APIName
    c.checkKeys(f, n, p, "apiName", "from", "to", "foreignKey", "joinTable")
    if !reMemberName.MatchString(r.APIName) {
        c.errf("G003", fieldPos(f, n, "apiName"), p+".apiName", "link type apiName %q must match %s", r.APIName, reMemberName)
    }
    if prev, ok := c.out.LinkTypes[r.APIName]; ok {
        c.errf("G002", posOf(f, n), p, "duplicate link type %q (first defined at %s)", r.APIName, prev.Pos)
        return
    }
    lt := &LinkType{
        APIName:    r.APIName,
        From:       LinkSide{ObjectType: r.From.ObjectType, APIName: r.From.APIName, Cardinality: Cardinality(r.From.Cardinality)},
        To:         LinkSide{ObjectType: r.To.ObjectType, APIName: r.To.APIName, Cardinality: Cardinality(r.To.Cardinality)},
        ForeignKey: r.ForeignKey,
        JoinTable:  r.JoinTable,
        Pos:        posOf(f, n),
    }
    c.out.LinkTypes[r.APIName] = lt

    endsOK := true
    sides := []struct {
        key  string
        side LinkSide
    }{{"from", lt.From}, {"to", lt.To}}
    for _, s := range sides {
        sn := fieldNode(n, s.key)
        sp := p + "." + s.key
        c.checkKeys(f, sn, sp, "objectType", "apiName", "cardinality")
        if _, ok := c.out.ObjectTypes[s.side.ObjectType]; !ok {
            c.errf("LT001", fieldPos(f, sn, "objectType"), sp+".objectType", "object type %q not found%s", s.side.ObjectType, didYouMean(s.side.ObjectType, keys(c.out.ObjectTypes)))
            endsOK = false
        }
        if !reMemberName.MatchString(s.side.APIName) {
            c.errf("G003", fieldPos(f, sn, "apiName"), sp+".apiName", "link navigation name %q must match %s", s.side.APIName, reMemberName)
        }
        if s.side.Cardinality != CardOne && s.side.Cardinality != CardMany {
            c.errf("G005", fieldPos(f, sn, "cardinality"), sp+".cardinality", "cardinality must be ONE or MANY; got %q", s.side.Cardinality)
        }
    }
    if !endsOK {
        return
    }

    // LT002: the navigation name lives on the side's object type.
    for _, s := range sides {
        ot := c.out.ObjectTypes[s.side.ObjectType]
        sp := p + "." + s.key + ".apiName"
        pos := fieldPos(f, fieldNode(n, s.key), "apiName")
        if _, isProp := ot.Properties[s.side.APIName]; isProp {
            c.errf("LT002", pos, sp, "navigation name %q conflicts with a property of %s", s.side.APIName, ot.APIName)
            continue
        }
        names := c.linkNames[ot.APIName]
        if names == nil {
            names = map[string]string{}
            c.linkNames[ot.APIName] = names
        }
        if other, ok := names[s.side.APIName]; ok {
            c.errf("LT002", pos, sp, "navigation name %q on %s is already used by link %q", s.side.APIName, ot.APIName, other)
            continue
        }
        names[s.side.APIName] = lt.APIName
    }

    hasFK, hasJT := lt.ForeignKey != nil, lt.JoinTable != nil
    m2m := lt.From.Cardinality == CardMany && lt.To.Cardinality == CardMany
    switch {
    case hasFK == hasJT:
        c.errf("LT003", posOf(f, n), p, "exactly one of foreignKey or joinTable must be set")
        return
    case m2m && hasFK:
        c.errf("LT003", fieldPos(f, n, "foreignKey"), p+".foreignKey", "many-to-many links must use joinTable")
        return
    case !m2m && hasJT:
        c.errf("LT003", fieldPos(f, n, "joinTable"), p+".joinTable", "one-to-many / one-to-one links must use foreignKey")
        return
    }

    if hasJT {
        jt := lt.JoinTable
        jn := fieldNode(n, "joinTable")
        c.checkKeys(f, jn, p+".joinTable", "connector", "table", "fromColumn", "toColumn")
        if _, ok := c.out.Connectors[jt.Connector]; !ok {
            c.errf("LT005", fieldPos(f, jn, "connector"), p+".joinTable.connector", "connector %q is not declared in ontology.connectors%s", jt.Connector, didYouMean(jt.Connector, keys(c.out.Connectors)))
        }
        if jt.Table == "" || jt.FromColumn == "" || jt.ToColumn == "" {
            c.errf("G005", posOf(f, jn), p+".joinTable", "joinTable requires table, fromColumn and toColumn")
        }
        return
    }

    fk := lt.ForeignKey
    fn := fieldNode(n, "foreignKey")
    fp := p + ".foreignKey"
    c.checkKeys(f, fn, fp, "on", "property")
    var target string
    switch fk.On {
    case lt.From.ObjectType:
        target = lt.To.ObjectType
    case lt.To.ObjectType:
        target = lt.From.ObjectType
    default:
        c.errf("LT004", fieldPos(f, fn, "on"), fp+".on", "foreignKey.on must be one of the linked object types (%s, %s); got %q", lt.From.ObjectType, lt.To.ObjectType, fk.On)
        return
    }
    if lt.From.ObjectType != lt.To.ObjectType {
        fkSide := lt.From
        if fk.On == lt.To.ObjectType {
            fkSide = lt.To
        }
        if fkSide.Cardinality != CardOne {
            c.errf("LT004", fieldPos(f, fn, "on"), fp+".on", "the object holding the foreign key (%s) must navigate with cardinality ONE", fk.On)
        }
    }
    onOT := c.out.ObjectTypes[fk.On]
    fkProp, ok := onOT.Properties[fk.Property]
    if !ok {
        c.errf("LT004", fieldPos(f, fn, "property"), fp+".property", "property %q not found on %s%s", fk.Property, fk.On, didYouMean(fk.Property, onOT.PropertyOrder))
        return
    }
    tgt := c.out.ObjectTypes[target]
    if tgtPK, ok := tgt.Properties[tgt.PrimaryKey]; ok && fkProp.Type.Base != tgtPK.Type.Base {
        c.errf("LT004", fieldPos(f, fn, "property"), fp+".property", "foreign key %s.%s is %s but %s.%s (primary key) is %s", fk.On, fk.Property, fkProp.Type, tgt.APIName, tgt.PrimaryKey, tgtPK.Type)
    }
}

// checksum is the sha256 of the canonical JSON (map keys are sorted by encoding/json; positions excluded).
func checksum(o *CompiledOntology) (string, error) {
    cp := *o
    cp.Checksum = ""
    cp.Version = 0
    cp.GitCommit = ""
    b, err := json.Marshal(cp)
    if err != nil {
        return "", err
    }
    sum := sha256.Sum256(b)
    return hex.EncodeToString(sum[:]), nil
}
