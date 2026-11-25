package oms

import (
	"context"
	"net/http"
	"sort"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"
)

// Service runs the publish state machine and serves metadata. Spec §5.3.
type Service struct {
	store *Store
}

// NewService builds the OMS service.
func NewService(store *Store) *Service { return &Service{store: store} }

// PublishInput is a publish request: the source files and the committing identity.
type PublishInput struct {
	Ontology     string       `json:"ontology"`
	GitCommit    string       `json:"gitCommit"`
	Files        []SourceFile `json:"files"`
	DryRun       bool         `json:"dryRun"`
	Actor        string       `json:"-"`
	AllowTighten bool         `json:"allowConstraintTightening"`
}

// SourceFile is one YAML source as sent by ontoctl.
type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// PublishResult reports the outcome.
type PublishResult struct {
	Version     int64            `json:"version"`
	Status      string           `json:"status"`
	ChangeSet   spec.ChangeSet   `json:"changeset"`
	Diagnostics spec.Diagnostics `json:"diagnostics,omitempty"`
	Reused      bool             `json:"reused"`
}

// Publish runs the full state machine. The server re-compiles and re-validates — it never trusts
// the client's compiled output (Spec §5.5).
func (s *Service) Publish(ctx context.Context, in PublishInput) (*PublishResult, error) {
	// [1] compile + validate
	ont, diags := compileFiles(in.Files)
	if diags.HasErrors(false) {
		return &PublishResult{Status: StatusFailed, Diagnostics: diags},
			apperr.BadRequest("VALIDATION_FAILED", "ontology failed validation").WithDetails(map[string]any{"diagnostics": diags})
	}
	if in.Ontology != "" && ont.APIName != in.Ontology {
		return nil, apperr.BadRequest("ONTOLOGY_MISMATCH", "request ontology %q does not match compiled apiName %q", in.Ontology, ont.APIName)
	}
	ont.GitCommit = in.GitCommit

	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := s.store.EnsureOntology(ctx, tx, ont.APIName, ont.DisplayName); err != nil {
		return nil, err
	}
	activeVer, err := s.store.LockOntology(ctx, tx, ont.APIName)
	if err != nil {
		return nil, err
	}

	// [2] idempotent publish by checksum
	if existing, err := s.store.FindByChecksum(ctx, tx, ont.APIName, ont.Checksum); err != nil {
		return nil, err
	} else if existing != nil {
		_ = tx.Commit(ctx)
		return &PublishResult{Version: existing.Version, Status: existing.Status, Reused: true}, nil
	}

	// [4] diff against active, enforce migration coverage (PUB002)
	var oldOnt *spec.CompiledOntology
	if activeVer != nil {
		if oldOnt, err = s.store.GetCompiled(ctx, ont.APIName, *activeVer); err != nil {
			return nil, err
		}
	}
	cs := spec.Diff(oldOnt, ont)
	missing := spec.MigrationCoverage(cs, ont.Migrations)
	blocking := make([]spec.Change, 0, len(missing))
	for _, c := range missing {
		if c.Kind == spec.TightenConstraint && in.AllowTighten {
			continue
		}
		blocking = append(blocking, c)
	}
	if len(blocking) > 0 {
		return &PublishResult{Status: StatusFailed, ChangeSet: cs},
			apperr.Conflict("PUB002", "%d breaking change(s) require a migration", len(blocking)).
				WithDetails(map[string]any{"missingMigrations": blocking})
	}

	if in.DryRun {
		_ = tx.Rollback(ctx)
		return &PublishResult{Version: derefOr(activeVer, 0), Status: "DRY_RUN", ChangeSet: cs}, nil
	}

	// [5] assign version + RIDs, persist PENDING
	version, err := s.store.NextVersion(ctx, tx, ont.APIName)
	if err != nil {
		return nil, err
	}
	ont.Version = version
	rids, err := s.store.AssignRIDs(ctx, tx, ont.APIName, version, ridItems(ont))
	if err != nil {
		return nil, err
	}
	stampRIDs(ont, rids)
	// recompute checksum excludes version/commit, so stamping RIDs is fine for the stored blob
	if err := s.store.InsertVersion(ctx, tx, &Version{
		Ontology: ont.APIName, Version: version, GitCommit: in.GitCommit, Checksum: ont.Checksum,
		Compiled: ont, ChangeSet: cs, PublishedBy: in.Actor,
	}); err != nil {
		return nil, err
	}
	// [6] commit the PENDING row so DDL runs against a durable version
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// [7] apply DDL in its own transaction
	if err := s.applyDDL(ctx, oldOnt, ont); err != nil {
		_ = s.store.MarkStatus(ctx, ont.APIName, version, StatusFailed, map[string]any{"ddl": err.Error()})
		return &PublishResult{Version: version, Status: StatusFailed},
			apperr.New(http.StatusInternalServerError, "PUB003", "failed to apply DDL").Wrap(err)
	}
	_ = s.store.MarkDDLApplied(ctx, ont.APIName, version)

	// [8] activate
	if err := s.activate(ctx, ont.APIName, version); err != nil {
		return nil, err
	}
	return &PublishResult{Version: version, Status: StatusActive, ChangeSet: cs}, nil
}

func (s *Service) applyDDL(ctx context.Context, old, new *spec.CompiledOntology) error {
	stmts, err := objstore.PlanDDL(old, new)
	if err != nil {
		return err
	}
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) activate(ctx context.Context, apiName string, version int64) error {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := s.store.Activate(ctx, tx, apiName, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ActivateVersion is used by the :activate endpoint for rollback. It refuses an activation
// that would require dropping columns (DDL is additive-only in MVP).
func (s *Service) ActivateVersion(ctx context.Context, apiName string, version int64) error {
	target, err := s.store.GetCompiled(ctx, apiName, version)
	if err != nil {
		return err
	}
	if target == nil {
		return apperr.NotFound("version %d of ontology %q not found", version, apiName)
	}
	active, err := s.store.GetActiveCompiled(ctx, apiName)
	if err != nil && err != ErrNoActive {
		return err
	}
	// rollback must not need destructive DDL
	if _, err := objstore.PlanDDL(active, target); err != nil {
		return apperr.Conflict("ROLLBACK_INCOMPATIBLE", "cannot activate: %v", err)
	}
	return s.activate(ctx, apiName, version)
}

func ridItems(o *spec.CompiledOntology) []RIDItem {
	var items []RIDItem
	add := func(kind string, names []string) {
		sort.Strings(names)
		for _, n := range names {
			items = append(items, RIDItem{Kind: kind, APIName: n})
		}
	}
	add("object-type", keysOf(o.ObjectTypes))
	add("link-type", keysOf(o.LinkTypes))
	add("action-type", keysOf(o.ActionTypes))
	add("value-type", keysOf(o.ValueTypes))
	return items
}

func stampRIDs(o *spec.CompiledOntology, rids map[string]string) {
	for n, ot := range o.ObjectTypes {
		ot.RID = rids["object-type/"+n]
	}
	for n, lt := range o.LinkTypes {
		lt.RID = rids["link-type/"+n]
	}
	for n, at := range o.ActionTypes {
		at.RID = rids["action-type/"+n]
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func derefOr(p *int64, d int64) int64 {
	if p == nil {
		return d
	}
	return *p
}
