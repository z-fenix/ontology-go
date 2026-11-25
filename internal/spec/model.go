package spec

// BaseType is a primitive or container property type.
type BaseType string

const (
	TString     BaseType = "string"
	TInteger    BaseType = "integer"
	TLong       BaseType = "long"
	TDouble     BaseType = "double"
	TDecimal    BaseType = "decimal"
	TBoolean    BaseType = "boolean"
	TDate       BaseType = "date"
	TTimestamp  BaseType = "timestamp"
	TGeoPoint   BaseType = "geopoint"
	TVector     BaseType = "vector"
	TAttachment BaseType = "attachment"
	TArray      BaseType = "array"
	TStruct     BaseType = "struct"
)

// IsScalar reports whether the type can carry value-type constraints.
func (b BaseType) IsScalar() bool {
	switch b {
	case TString, TInteger, TLong, TDouble, TDecimal, TBoolean, TDate, TTimestamp:
		return true
	}
	return false
}

// IsNumeric reports whether min/max apply numerically.
func (b BaseType) IsNumeric() bool {
	switch b {
	case TInteger, TLong, TDouble, TDecimal:
		return true
	}
	return false
}

// IsTemporal reports whether the type is a date or timestamp.
func (b BaseType) IsTemporal() bool { return b == TDate || b == TTimestamp }

// TypeRef is a parsed property type such as "vector(1024)" or "array<string>".
type TypeRef struct {
	Base      BaseType `json:"base"`
	Dim       int      `json:"dim,omitempty"`
	Precision int      `json:"precision,omitempty"`
	Scale     int      `json:"scale,omitempty"`
	Elem      *TypeRef `json:"elem,omitempty"`
}

// Constraints restrict the values of a scalar type.
type Constraints struct {
	Regex  *string  `yaml:"regex,omitempty" json:"regex,omitempty"`
	Enum   []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Min    *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max    *float64 `yaml:"max,omitempty" json:"max,omitempty"`
	MinLen *int     `yaml:"minLength,omitempty" json:"minLength,omitempty"`
	MaxLen *int     `yaml:"maxLength,omitempty" json:"maxLength,omitempty"`
}

// IsZero reports whether no constraint is set.
func (c Constraints) IsZero() bool {
	return c.Regex == nil && len(c.Enum) == 0 && c.Min == nil && c.Max == nil && c.MinLen == nil && c.MaxLen == nil
}

// Sensitivity classifies a property for redaction.
type Sensitivity string

const (
	SensPublic       Sensitivity = "PUBLIC"
	SensInternal     Sensitivity = "INTERNAL"
	SensConfidential Sensitivity = "CONFIDENTIAL"
	SensRestricted   Sensitivity = "RESTRICTED"
)

// Valid reports whether s is a known sensitivity level.
func (s Sensitivity) Valid() bool {
	switch s {
	case SensPublic, SensInternal, SensConfidential, SensRestricted:
		return true
	}
	return false
}

// ConflictStrategy decides how user edits and datasource updates merge.
type ConflictStrategy string

const (
	ApplyUserEdits  ConflictStrategy = "APPLY_USER_EDITS"
	MostRecentValue ConflictStrategy = "MOST_RECENT_VALUE"
)

// Connector is a named source system declared in ontology.yaml.
type Connector struct {
	Kind   string `yaml:"kind" json:"kind"`
	DSNRef string `yaml:"dsnRef" json:"dsnRef"`
}

// ValueType is a reusable constrained semantic type.
type ValueType struct {
	APIName     string      `json:"apiName"`
	BaseType    BaseType    `json:"baseType"`
	Constraints Constraints `json:"constraints"`
	Pos         SourcePos   `json:"-"`
}

// Property of an object type.
type Property struct {
	APIName     string      `json:"apiName"`
	Type        TypeRef     `json:"type"`
	Required    bool        `json:"required"`
	ValueType   string      `json:"valueType,omitempty"`
	Constraints Constraints `json:"constraints"`
	Sensitivity Sensitivity `json:"sensitivity"`
	DerivedFrom string      `json:"derivedFrom,omitempty"`
	Column      string      `json:"column"`
	Pos         SourcePos   `json:"-"`
}

// Datasource maps an object type to a source table.
type Datasource struct {
	Connector        string            `json:"connector"`
	Table            string            `json:"table"`
	ColumnMapping    map[string]string `json:"columnMapping,omitempty"`
	ConflictStrategy ConflictStrategy  `json:"conflictStrategy"`
	TimestampColumn  string            `json:"timestampColumn,omitempty"`
}

// AuthzTuple derives an OpenFGA relationship tuple from object data.
type AuthzTuple struct {
	Relation string `yaml:"relation" json:"relation"`
	Target   string `yaml:"target" json:"target"`
}

// ObjectAuthz binds an object type to an OpenFGA type.
type ObjectAuthz struct {
	FGAType string       `yaml:"fgaType" json:"fgaType"`
	Tuples  []AuthzTuple `yaml:"tuples" json:"tuples"`
}

// ObjectType is a business entity type.
type ObjectType struct {
	RID           string               `json:"rid,omitempty"`
	APIName       string               `json:"apiName"`
	DisplayName   string               `json:"displayName"`
	PrimaryKey    string               `json:"primaryKey"`
	TitleProperty string               `json:"titleProperty"`
	Properties    map[string]*Property `json:"properties"`
	PropertyOrder []string             `json:"propertyOrder"`
	Datasource    *Datasource          `json:"datasource,omitempty"`
	Authz         *ObjectAuthz         `json:"authz,omitempty"`
	TableName     string               `json:"tableName"`
	Pos           SourcePos            `json:"-"`
}

// Cardinality of one side of a link.
type Cardinality string

const (
	CardOne  Cardinality = "ONE"
	CardMany Cardinality = "MANY"
)

// LinkSide: APIName is the navigation name on ObjectType; Cardinality is how many targets it yields.
type LinkSide struct {
	ObjectType  string      `json:"objectType"`
	APIName     string      `json:"apiName"`
	Cardinality Cardinality `json:"cardinality"`
}

// ForeignKey implements a 1:N link.
type ForeignKey struct {
	On       string `yaml:"on" json:"on"`
	Property string `yaml:"property" json:"property"`
}

// JoinTable implements an M:N link.
type JoinTable struct {
	Connector  string `yaml:"connector" json:"connector"`
	Table      string `yaml:"table" json:"table"`
	FromColumn string `yaml:"fromColumn" json:"fromColumn"`
	ToColumn   string `yaml:"toColumn" json:"toColumn"`
}

// LinkType is a relationship between two object types.
type LinkType struct {
	RID        string      `json:"rid,omitempty"`
	APIName    string      `json:"apiName"`
	From       LinkSide    `json:"from"`
	To         LinkSide    `json:"to"`
	ForeignKey *ForeignKey `json:"foreignKey,omitempty"`
	JoinTable  *JoinTable  `json:"joinTable,omitempty"`
	Pos        SourcePos   `json:"-"`
}

// CompiledOntology is the unit stored and distributed by OMS.
type CompiledOntology struct {
	APIName     string                 `json:"apiName"`
	DisplayName string                 `json:"displayName"`
	Version     int64                  `json:"version"`
	GitCommit   string                 `json:"gitCommit,omitempty"`
	Connectors  map[string]Connector   `json:"connectors"`
	ValueTypes  map[string]*ValueType  `json:"valueTypes"`
	ObjectTypes map[string]*ObjectType `json:"objectTypes"`
	LinkTypes   map[string]*LinkType   `json:"linkTypes"`
	ActionTypes map[string]*ActionType `json:"actionTypes"`
	Webhooks    map[string]*Webhook    `json:"webhooks"`
	Migrations  []Migration            `json:"migrations,omitempty"`
	Checksum    string                 `json:"checksum,omitempty"`
}
