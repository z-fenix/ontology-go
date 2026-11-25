// Package oms implements the Ontology Metadata Service: the publish state machine,
// metadata queries, and version/RID assignment described in Spec §5.
package oms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acme/ontology/internal/spec"
)

// DB is the subset of pgx used by the store (satisfied by *pgxpool.Pool and pgx.Tx).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Status values for an ontology version.
const (
	StatusPending    = "PENDING"
	StatusActive     = "ACTIVE"
	StatusSuperseded = "SUPERSEDED"
	StatusFailed     = "FAILED"
	StatusRolledBack = "ROLLED_BACK"
)

// Version is a stored ontology version row.
type Version struct {
	Ontology    string                 `json:"ontology"`
	Version     int64                  `json:"version"`
	GitCommit   string                 `json:"gitCommit"`
	Checksum    string                 `json:"checksum"`
	Status      string                 `json:"status"`
	Compiled    *spec.CompiledOntology `json:"compiled,omitempty"`
	ChangeSet   spec.ChangeSet         `json:"changeset"`
	PublishedBy string                 `json:"publishedBy"`
	PublishedAt time.Time              `json:"publishedAt"`
}

// Store persists ontology metadata.
type Store struct{ db DB }

// NewStore returns a Store over db.
func NewStore(db DB) *Store { return &Store{db: db} }

// ErrNoActive indicates an ontology exists but has no active version yet.
var ErrNoActive = errors.New("no active version")

// EnsureOntology inserts the ontology row if absent.
func (s *Store) EnsureOntology(ctx context.Context, tx DB, apiName, displayName string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO oms.ontology (api_name, display_name) VALUES ($1, $2) ON CONFLICT (api_name) DO NOTHING`,
		apiName, displayName)
	return err
}

// LockOntology serializes publishes by locking the ontology row (FOR UPDATE).
func (s *Store) LockOntology(ctx context.Context, tx DB, apiName string) (activeVersion *int64, err error) {
	var v *int64
	err = tx.QueryRow(ctx, `SELECT active_version FROM oms.ontology WHERE api_name = $1 FOR UPDATE`, apiName).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActive
	}
	return v, err
}

// FindByChecksum returns an existing version with the same checksum (idempotent publish), or nil.
func (s *Store) FindByChecksum(ctx context.Context, tx DB, apiName, checksum string) (*Version, error) {
	row := tx.QueryRow(ctx,
		`SELECT version, status FROM oms.ontology_version WHERE ontology = $1 AND checksum = $2`, apiName, checksum)
	var v Version
	v.Ontology, v.Checksum = apiName, checksum
	err := row.Scan(&v.Version, &v.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// NextVersion returns max(version)+1 for the ontology (1 when none exist).
func (s *Store) NextVersion(ctx context.Context, tx DB, apiName string) (int64, error) {
	var max *int64
	if err := tx.QueryRow(ctx, `SELECT max(version) FROM oms.ontology_version WHERE ontology = $1`, apiName).Scan(&max); err != nil {
		return 0, err
	}
	if max == nil {
		return 1, nil
	}
	return *max + 1, nil
}

// GetActiveCompiled returns the active compiled ontology, or ErrNoActive.
func (s *Store) GetActiveCompiled(ctx context.Context, apiName string) (*spec.CompiledOntology, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `
SELECT v.compiled FROM oms.ontology o
JOIN oms.ontology_version v ON v.ontology = o.api_name AND v.version = o.active_version
WHERE o.api_name = $1`, apiName).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActive
	}
	if err != nil {
		return nil, err
	}
	return decodeCompiled(raw)
}

// GetCompiled returns a specific version's compiled ontology.
func (s *Store) GetCompiled(ctx context.Context, apiName string, version int64) (*spec.CompiledOntology, error) {
	var raw []byte
	err := s.db.QueryRow(ctx,
		`SELECT compiled FROM oms.ontology_version WHERE ontology = $1 AND version = $2`, apiName, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeCompiled(raw)
}

func decodeCompiled(raw []byte) (*spec.CompiledOntology, error) {
	var c spec.CompiledOntology
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode compiled ontology: %w", err)
	}
	return &c, nil
}

// ListVersions returns version metadata (newest first), without the compiled blob.
func (s *Store) ListVersions(ctx context.Context, apiName string, limit int) ([]Version, error) {
	rows, err := s.db.Query(ctx, `
SELECT version, git_commit, checksum, status, published_by, published_at
FROM oms.ontology_version WHERE ontology = $1 ORDER BY version DESC LIMIT $2`, apiName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v := Version{Ontology: apiName}
		if err := rows.Scan(&v.Version, &v.GitCommit, &v.Checksum, &v.Status, &v.PublishedBy, &v.PublishedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// InsertVersion writes a PENDING version row.
func (s *Store) InsertVersion(ctx context.Context, tx DB, v *Version) error {
	compiled, err := json.Marshal(v.Compiled)
	if err != nil {
		return err
	}
	changeset, err := json.Marshal(v.ChangeSet)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO oms.ontology_version (ontology, version, git_commit, checksum, compiled, changeset, status, published_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		v.Ontology, v.Version, v.GitCommit, v.Checksum, compiled, changeset, StatusPending, v.PublishedBy)
	return err
}

// AssignRIDs reuses the RID for an existing (ontology, kind, apiName) or mints a new one.
// Returns a map "<kind>/<apiName>" -> rid for the caller to stamp into the compiled blob.
func (s *Store) AssignRIDs(ctx context.Context, tx DB, apiName string, version int64, items []RIDItem) (map[string]string, error) {
	out := make(map[string]string, len(items))
	for _, it := range items {
		var rid string
		err := tx.QueryRow(ctx, `
INSERT INTO oms.type_registry (ontology, kind, api_name, rid, first_version)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (ontology, kind, api_name) DO UPDATE SET last_version = NULL
RETURNING rid`, apiName, it.Kind, it.APIName, newRID(it.Kind), version).Scan(&rid)
		if err != nil {
			return nil, err
		}
		out[it.Kind+"/"+it.APIName] = rid
	}
	return out, nil
}

// RIDItem names a type needing a RID.
type RIDItem struct{ Kind, APIName string }

// Activate makes version the active one and supersedes the previous active version.
func (s *Store) Activate(ctx context.Context, tx DB, apiName string, version int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE oms.ontology_version SET status = $1 WHERE ontology = $2 AND status = $3`,
		StatusSuperseded, apiName, StatusActive); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE oms.ontology_version SET status = $1 WHERE ontology = $2 AND version = $3`,
		StatusActive, apiName, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE oms.ontology SET active_version = $1 WHERE api_name = $2`, version, apiName); err != nil {
		return err
	}
	// Spec §5.1: notify listeners of the version change.
	_, err := tx.Exec(ctx, `SELECT pg_notify('oms_version', $1)`, fmt.Sprintf("%s:%d", apiName, version))
	return err
}

// MarkStatus sets a version's status (e.g. FAILED), outside the publish transaction.
func (s *Store) MarkStatus(ctx context.Context, apiName string, version int64, status string, errInfo any) error {
	var errJSON []byte
	if errInfo != nil {
		errJSON, _ = json.Marshal(errInfo)
	}
	_, err := s.db.Exec(ctx,
		`UPDATE oms.ontology_version SET status = $1, error = $2 WHERE ontology = $3 AND version = $4`,
		status, errJSON, apiName, version)
	return err
}

// MarkDDLApplied records the DDL completion time.
func (s *Store) MarkDDLApplied(ctx context.Context, apiName string, version int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE oms.ontology_version SET ddl_applied_at = now() WHERE ontology = $1 AND version = $2`, apiName, version)
	return err
}

// RawDB exposes the underlying DB for DDL execution by the service layer.
func (s *Store) RawDB() DB { return s.db }
