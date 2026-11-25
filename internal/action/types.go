// Package action implements the Action Service engine (Spec §7).
package action

import (
	"context"
	"errors"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// ActorKind distinguishes humans, agents and services.
type ActorKind string

const (
	ActorUser    ActorKind = "USER"
	ActorAgent   ActorKind = "AGENT"
	ActorService ActorKind = "SERVICE"
)

// Actor is the authenticated caller.
type Actor struct {
	ID         string
	Kind       ActorKind
	OnBehalfOf string // required for agents (the user the agent acts for)
	Groups     []string
}

// Mode selects apply vs validate-only.
type Mode int

const (
	ModeApply Mode = iota
	ModeValidate
)

// ApplyRequest is one action invocation.
type ApplyRequest struct {
	ActionType      string
	Parameters      map[string]any
	OntologyVersion *int64
	IdempotencyKey  string
	Actor           Actor
	Mode            Mode
}

// CriterionResult is the outcome of one submission criterion.
type CriterionResult struct {
	Index   int    `json:"index"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

// Validation summarizes criteria evaluation.
type Validation struct {
	Result   string            `json:"result"` // VALID | INVALID
	Criteria []CriterionResult `json:"criteria"`
}

// RefJSON is an object reference in API responses.
type RefJSON struct {
	Type string `json:"type"`
	PK   string `json:"pk"`
}

// LinkJSON is a link edit in API responses.
type LinkJSON struct {
	Op       string  `json:"op"`
	LinkType string  `json:"linkType"`
	From     RefJSON `json:"from"`
	To       RefJSON `json:"to"`
}

// EditSummary lists what an action changed.
type EditSummary struct {
	Created  []RefJSON  `json:"created"`
	Modified []RefJSON  `json:"modified"`
	Deleted  []RefJSON  `json:"deleted"`
	Links    []LinkJSON `json:"links"`
}

// ApplyResult is returned to clients and stored in action_run.result.
type ApplyResult struct {
	ActionRID        string      `json:"actionRid,omitempty"`
	Status           string      `json:"status"`
	OntologyVersion  int64       `json:"ontologyVersion"`
	Validation       Validation  `json:"validation"`
	Edits            EditSummary `json:"edits"`
	ConsistencyToken string      `json:"consistencyToken,omitempty"`
}

// Run status values (Spec §7.2).
const (
	StatusValid     = "VALID"
	StatusCommitted = "COMMITTED"
)

// OntologySource resolves compiled ontologies (satisfied by *omsclient.Cache).
type OntologySource interface {
	Active(ctx context.Context) (*spec.CompiledOntology, error)
	Get(ctx context.Context, version int64) (*spec.CompiledOntology, error)
}

// ObjectLoader reads objects (satisfied by *objstore.Store).
type ObjectLoader interface {
	GetMany(ctx context.Context, ont *spec.CompiledOntology, refs []objstore.ObjectRef) (map[objstore.ObjectRef]*objstore.Object, error)
	Find(ctx context.Context, ont *spec.CompiledOntology, typ string, f objstore.Filter, page objstore.Page) ([]*objstore.Object, string, error)
}

// CommitInput is everything the committer writes in one transaction.
type CommitInput struct {
	Ontology       *spec.CompiledOntology
	ActionRID      string
	ActionType     string
	Actor          Actor
	IdempotencyKey string
	RequestHash    string
	Parameters     map[string]any
	Edits          []objstore.Edit
	Before         map[objstore.ObjectRef]map[string]any
	Effects        []PlannedEffect // rendered side effects; recorded as PENDING side_effect_run rows
	Result         *ApplyResult    // committer fills ConsistencyToken and stores it
}

// PriorRun is an earlier run found by idempotency key.
type PriorRun struct {
	ActionRID   string
	RequestHash string
	Status      string
	Result      *ApplyResult
}

// SideEffectRecord is a side_effect_run row.
type SideEffectRecord struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

// RunRecord is an action_run row for GET /actionRuns/{rid}.
type RunRecord struct {
	ActionRID   string             `json:"actionRid"`
	ActionType  string             `json:"actionType"`
	ActorID     string             `json:"actor"`
	Status      string             `json:"status"`
	Result      *ApplyResult       `json:"result,omitempty"`
	Error       map[string]any     `json:"error,omitempty"`
	SideEffects []SideEffectRecord `json:"sideEffects"`
}

// Committer persists an action atomically (PG implementation in pgcommit.go).
type Committer interface {
	FindByIdempotency(ctx context.Context, actorID, actionType, key string) (*PriorRun, error)
	// Commit applies edits and records the run in one transaction.
	Commit(ctx context.Context, in *CommitInput) (*ApplyResult, error)
	// Reserve records a write-back run as WRITEBACK_PENDING with its plan; nothing is edited yet.
	Reserve(ctx context.Context, in *CommitInput) error
	// MarkRun sets a run's status and error (e.g. FAILED when its workflow cannot be started).
	MarkRun(ctx context.Context, actionRID, status, code, message string) error
	GetRun(ctx context.Context, actionRID string) (*RunRecord, error)
}

// ErrIdempotencyRace is returned by Commit when a concurrent request with the same
// idempotency key committed first; the engine then replays that run.
var ErrIdempotencyRace = errors.New("idempotency key already used by a concurrent request")
