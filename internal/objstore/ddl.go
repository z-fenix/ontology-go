// Package objstore reads and writes materialized Ontology objects in PostgreSQL.
package objstore

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/acme/ontology/internal/spec"
)

// Schema is the PostgreSQL schema holding materialized object tables.
const Schema = "obj"

const maxIdentLen = 63

var systemColumns = []string{
	`"_pk" TEXT PRIMARY KEY`,
	`"_rid" TEXT NOT NULL UNIQUE`,
	`"_version" BIGINT NOT NULL DEFAULT 1`,
	`"_source_ts" TIMESTAMPTZ`,
	`"_edit_ts" TIMESTAMPTZ`,
	`"_edited_props" TEXT[] NOT NULL DEFAULT '{}'`,
	`"_deleted" BOOLEAN NOT NULL DEFAULT false`,
	`"_created_at" TIMESTAMPTZ NOT NULL DEFAULT now()`,
	`"_updated_at" TIMESTAMPTZ NOT NULL DEFAULT now()`,
}

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func splitTable(name string) (schema, table string) {
	if s, t, ok := strings.Cut(name, "."); ok {
		return s, t
	}
	return Schema, name
}

func tableIdent(name string) string {
	s, t := splitTable(name)
	return ident(s, t)
}

// ColumnName is the materialized column for a property (snake_case of its apiName).
// Note: spec.Property.Column is the column in the *source* table, not this one.
func ColumnName(p *spec.Property) string { return spec.ToSnake(p.APIName) }

// Stored reports whether a property is materialized in PostgreSQL (vectors live only in the search index).
func Stored(p *spec.Property) bool { return p.Type.Base != spec.TVector }

// PGType maps an ontology type to a PostgreSQL column type.
func PGType(t spec.TypeRef) (string, error) {
	switch t.Base {
	case spec.TString, spec.TAttachment:
		return "TEXT", nil
	case spec.TInteger:
		return "INTEGER", nil
	case spec.TLong:
		return "BIGINT", nil
	case spec.TDouble:
		return "DOUBLE PRECISION", nil
	case spec.TDecimal:
		if t.Precision == 0 {
			return "NUMERIC", nil
		}
		return fmt.Sprintf("NUMERIC(%d,%d)", t.Precision, t.Scale), nil
	case spec.TBoolean:
		return "BOOLEAN", nil
	case spec.TDate:
		return "DATE", nil
	case spec.TTimestamp:
		return "TIMESTAMPTZ", nil
	case spec.TGeoPoint, spec.TArray, spec.TStruct:
		return "JSONB", nil
	}
	return "", fmt.Errorf("type %s is not stored in PostgreSQL", t)
}

// LinkTableName is the materialized table of a many-to-many link type.
func LinkTableName(lt *spec.LinkType) string { return Schema + ".link_" + spec.ToSnake(lt.APIName) }

func isManyToMany(lt *spec.LinkType) bool { return lt.JoinTable != nil }

func indexName(table, col string) string {
	name := "ix_" + table + "_" + col
	if len(name) <= maxIdentLen {
		return name
	}
	sum := sha1.Sum([]byte(name))
	return name[:maxIdentLen-9] + "_" + hex.EncodeToString(sum[:4])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// storedColumns returns (property, column) pairs in declaration order, rejecting column collisions.
func storedColumns(ot *spec.ObjectType) ([]column, error) {
	seen := map[string]string{}
	var cols []column
	for _, name := range ot.PropertyOrder {
		p := ot.Properties[name]
		if !Stored(p) {
			continue
		}
		col := ColumnName(p)
		if other, dup := seen[col]; dup {
			return nil, fmt.Errorf("%s: properties %q and %q both map to column %q", ot.APIName, other, name, col)
		}
		seen[col] = name
		cols = append(cols, column{prop: p, name: col})
	}
	return cols, nil
}

func createObjectTable(ot *spec.ObjectType) (string, error) {
	cols, err := storedColumns(ot)
	if err != nil {
		return "", err
	}
	defs := append([]string(nil), systemColumns...)
	for _, c := range cols {
		typ, err := PGType(c.prop.Type)
		if err != nil {
			return "", fmt.Errorf("%s.%s: %w", ot.APIName, c.prop.APIName, err)
		}
		defs = append(defs, ident(c.name)+" "+typ)
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", tableIdent(ot.TableName), strings.Join(defs, ",\n  ")), nil
}

func createLinkTable(lt *spec.LinkType) []string {
	_, table := splitTable(LinkTableName(lt))
	t := tableIdent(LinkTableName(lt))
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  "from_pk" TEXT NOT NULL,
  "to_pk" TEXT NOT NULL,
  "_source" TEXT NOT NULL CHECK ("_source" IN ('DATASOURCE','EDIT')),
  "_deleted" BOOLEAN NOT NULL DEFAULT false,
  "_updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY ("from_pk", "to_pk")
)`, t),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", ident(indexName(table, "to_pk")), t, ident("to_pk")),
	}
}

// linkDDL returns foreign-key indexes and many-to-many tables (all idempotent).
func linkDDL(ont *spec.CompiledOntology) ([]string, error) {
	var stmts []string
	for _, name := range sortedKeys(ont.LinkTypes) {
		lt := ont.LinkTypes[name]
		if isManyToMany(lt) {
			stmts = append(stmts, createLinkTable(lt)...)
			continue
		}
		if lt.ForeignKey == nil {
			continue
		}
		ot, ok := ont.ObjectTypes[lt.ForeignKey.On]
		if !ok {
			return nil, fmt.Errorf("link %s: unknown object type %q", name, lt.ForeignKey.On)
		}
		p, ok := ot.Properties[lt.ForeignKey.Property]
		if !ok {
			return nil, fmt.Errorf("link %s: unknown property %s.%s", name, ot.APIName, lt.ForeignKey.Property)
		}
		_, table := splitTable(ot.TableName)
		col := ColumnName(p)
		stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)",
			ident(indexName(table, col)), tableIdent(ot.TableName), ident(col)))
	}
	return stmts, nil
}

// GenerateDDL returns idempotent statements creating all object tables, link tables and indexes.
func GenerateDDL(ont *spec.CompiledOntology) ([]string, error) {
	return PlanDDL(nil, ont)
}

// PlanDDL returns statements migrating the physical schema from old to new (old may be nil).
// Only additive changes are planned; a changed column type returns an error (needs a castProperty
// migration, S2). Removed properties keep their column (soft delete, see Spec §5.3).
func PlanDDL(old, new *spec.CompiledOntology) ([]string, error) {
	stmts := []string{"CREATE SCHEMA IF NOT EXISTS " + ident(Schema)}
	for _, name := range sortedKeys(new.ObjectTypes) {
		ot := new.ObjectTypes[name]
		var prev *spec.ObjectType
		if old != nil {
			prev = old.ObjectTypes[name]
		}
		if prev == nil {
			s, err := createObjectTable(ot)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, s)
			continue
		}
		if prev.TableName != ot.TableName {
			return nil, fmt.Errorf("%s: table name changed from %s to %s", name, prev.TableName, ot.TableName)
		}
		cols, err := storedColumns(ot)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			typ, err := PGType(c.prop.Type)
			if err != nil {
				return nil, err
			}
			pp, existed := prev.Properties[c.prop.APIName]
			if !existed || !Stored(pp) {
				stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
					tableIdent(ot.TableName), ident(c.name), typ))
				continue
			}
			oldTyp, err := PGType(pp.Type)
			if err != nil {
				return nil, err
			}
			if oldTyp != typ {
				return nil, fmt.Errorf("%s.%s: column type change %s -> %s requires a castProperty migration",
					name, c.prop.APIName, oldTyp, typ)
			}
		}
	}
	links, err := linkDDL(new)
	if err != nil {
		return nil, err
	}
	return append(stmts, links...), nil
}
