package spec

// ParamKind classifies an action parameter.
type ParamKind string

const (
	KindPrimitive     ParamKind = "primitive"
	KindObjectRef     ParamKind = "objectRef"
	KindObjectRefList ParamKind = "objectRefList"
)

// ParamType is the resolved type of an action parameter.
type ParamType struct {
	Kind       ParamKind `json:"kind"`
	Base       BaseType  `json:"base,omitempty"`       // primitive
	ObjectType string    `json:"objectType,omitempty"` // objectRef / objectRefList
}

// ActionParam is a declared action parameter.
type ActionParam struct {
	APIName     string      `json:"apiName"`
	Type        ParamType   `json:"type"`
	Required    bool        `json:"required"`
	Default     any         `json:"default,omitempty"`
	ValueType   string      `json:"valueType,omitempty"`
	Constraints Constraints `json:"constraints"`
	Pos         SourcePos   `json:"-"`
}

// Criterion is a submission precondition (CEL expression returning bool).
type Criterion struct {
	Expr    string `yaml:"expr" json:"expr"`
	Message string `yaml:"message" json:"message"`
}

// RuleKind enumerates the built-in action rules.
type RuleKind string

const (
	RuleCreate RuleKind = "createObject"
	RuleModify RuleKind = "modifyObject"
	RuleDelete RuleKind = "deleteObject"
	RuleLink   RuleKind = "addLink"
	RuleUnlink RuleKind = "removeLink"
)

// Rule is one built-in edit within an action type.
type Rule struct {
	Kind       RuleKind          `json:"kind"`
	ObjectType string            `json:"objectType,omitempty"` // createObject
	Object     string            `json:"object,omitempty"`     // "{{param}}" for modify/delete
	Set        map[string]string `json:"set,omitempty"`        // property -> template/literal
	LinkType   string            `json:"linkType,omitempty"`   // addLink/removeLink
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	Pos        SourcePos         `json:"-"`
}

// WebhookMode selects when a webhook runs relative to the edit commit.
type WebhookMode string

const (
	WebhookWriteback  WebhookMode = "WRITEBACK"
	WebhookSideEffect WebhookMode = "SIDE_EFFECT"
)

// SideEffect is a post-validation effect of an action.
type SideEffect struct {
	Webhook     string      `json:"webhook,omitempty"`
	WebhookMode WebhookMode `json:"webhookMode,omitempty"`
	NotifyTo    string      `json:"notifyTo,omitempty"`
	Template    string      `json:"template,omitempty"`
	Pos         SourcePos   `json:"-"`
}

// ActionAI configures how an action is exposed to agents.
type ActionAI struct {
	ExposeToAgents           bool   `yaml:"exposeToAgents" json:"exposeToAgents"`
	AgentHint                string `yaml:"agentHint,omitempty" json:"agentHint,omitempty"`
	RequireHumanConfirmation bool   `yaml:"requireHumanConfirmation" json:"requireHumanConfirmation"`
}

// ActionPermission binds an action to an authorization check.
type ActionPermission struct {
	Relation string `yaml:"relation" json:"relation"`
	On       string `yaml:"on" json:"on"` // "{{param}}" referencing an objectRef parameter
}

// ActionFunction references a function-backed implementation.
type ActionFunction struct {
	Runtime string `yaml:"runtime" json:"runtime"` // typescript | python | go
	Ref     string `yaml:"ref" json:"ref"`
}

// ActionType is a governed write operation.
type ActionType struct {
	RID                string                  `json:"rid,omitempty"`
	APIName            string                  `json:"apiName"`
	DisplayName        string                  `json:"displayName"`
	Description        string                  `json:"description,omitempty"`
	Parameters         map[string]*ActionParam `json:"parameters"`
	ParamOrder         []string                `json:"paramOrder"`
	SubmissionCriteria []Criterion             `json:"submissionCriteria,omitempty"`
	Rules              []Rule                  `json:"rules,omitempty"`
	Function           *ActionFunction         `json:"function,omitempty"`
	SideEffects        []SideEffect            `json:"sideEffects,omitempty"`
	Permission         *ActionPermission       `json:"permission,omitempty"`
	AI                 ActionAI                `json:"ai"`
	Pos                SourcePos               `json:"-"`
}

// WebhookRetry configures webhook retry behavior.
type WebhookRetry struct {
	Max     int    `yaml:"max" json:"max"`
	Backoff string `yaml:"backoff" json:"backoff"` // exponential | fixed
}

// WebhookCompensate is the inverse call for a writeback webhook.
type WebhookCompensate struct {
	Method string         `yaml:"method" json:"method"`
	URL    string         `yaml:"url" json:"url"`
	Body   map[string]any `yaml:"body,omitempty" json:"body,omitempty"`
}

// WebhookAuth configures webhook authentication.
type WebhookAuth struct {
	Type      string `yaml:"type" json:"type"` // none | bearer | basic | oauth2_client_credentials
	SecretRef string `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
}

// Webhook is an external HTTP call usable as an action side effect.
type Webhook struct {
	Name       string             `yaml:"name" json:"name"`
	Method     string             `yaml:"method" json:"method"`
	URL        string             `yaml:"url" json:"url"`
	Auth       WebhookAuth        `yaml:"auth" json:"auth"`
	Body       map[string]any     `yaml:"body,omitempty" json:"body,omitempty"`
	TimeoutMs  int                `yaml:"timeoutMs" json:"timeoutMs"`
	Retry      WebhookRetry       `yaml:"retry" json:"retry"`
	Compensate *WebhookCompensate `yaml:"compensate,omitempty" json:"compensate,omitempty"`
	Pos        SourcePos          `yaml:"-" json:"-"`
}

// MigrationKind enumerates the supported schema migrations.
type MigrationKind string

const (
	MigRenameProperty   MigrationKind = "renameProperty"
	MigDropProperty     MigrationKind = "dropProperty"
	MigCastProperty     MigrationKind = "castProperty"
	MigDropObjectType   MigrationKind = "dropObjectType"
	MigRenameObjectType MigrationKind = "renameObjectType"
	MigBackfill         MigrationKind = "backfill"
	MigDropAllEdits     MigrationKind = "dropAllEdits"
)

// Migration declares an intentional breaking change between versions.
type Migration struct {
	Type       MigrationKind `yaml:"type" json:"type"`
	ObjectType string        `yaml:"objectType,omitempty" json:"objectType,omitempty"`
	From       string        `yaml:"from,omitempty" json:"from,omitempty"`
	To         string        `yaml:"to,omitempty" json:"to,omitempty"`
	Property   string        `yaml:"property,omitempty" json:"property,omitempty"`
	Default    any           `yaml:"default,omitempty" json:"default,omitempty"`
	Pos        SourcePos     `yaml:"-" json:"-"`
}
