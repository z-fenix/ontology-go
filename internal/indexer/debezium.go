package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// ChangeEvent is a decoded Debezium change event.
type ChangeEvent struct {
	Op     string         // c (create), u (update), d (delete), r (snapshot read)
	Table  string         // schema.table
	Before map[string]any // column -> value (nil for c/r)
	After  map[string]any // column -> value (nil for d)
	TS     time.Time      // source commit time
}

// ErrTombstone marks a Kafka tombstone (null value) that carries no change.
var ErrTombstone = errors.New("tombstone event")

// ParseDebezium decodes one Debezium JSON message value. Both the schema-enveloped form
// ({"schema":..,"payload":{..}}) and the bare payload form are accepted.
func ParseDebezium(value []byte) (*ChangeEvent, error) {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, ErrTombstone
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("debezium event: %w", err)
	}
	if p, ok := raw["payload"]; ok && !bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
		if _, hasOp := raw["op"]; !hasOp {
			raw = nil
			d := json.NewDecoder(bytes.NewReader(p))
			d.UseNumber()
			if err := d.Decode(&raw); err != nil {
				return nil, fmt.Errorf("debezium payload: %w", err)
			}
		}
	}
	var ev struct {
		Op     string         `json:"op"`
		Before map[string]any `json:"before"`
		After  map[string]any `json:"after"`
		Source struct {
			Schema string      `json:"schema"`
			Table  string      `json:"table"`
			TsMs   json.Number `json:"ts_ms"`
		} `json:"source"`
		TsMs json.Number `json:"ts_ms"`
	}
	b, _ := json.Marshal(raw)
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&ev); err != nil {
		return nil, fmt.Errorf("debezium event: %w", err)
	}
	switch ev.Op {
	case "c", "u", "d", "r":
	default:
		return nil, fmt.Errorf("debezium event: unsupported op %q", ev.Op)
	}
	out := &ChangeEvent{Op: ev.Op, Before: ev.Before, After: ev.After, TS: time.Now().UTC()}
	if ev.Source.Table != "" {
		out.Table = ev.Source.Table
		if ev.Source.Schema != "" {
			out.Table = ev.Source.Schema + "." + ev.Source.Table
		}
	}
	for _, ms := range []json.Number{ev.Source.TsMs, ev.TsMs} {
		if n, err := ms.Int64(); err == nil && n > 0 {
			out.TS = time.UnixMilli(n).UTC()
			break
		}
	}
	return out, nil
}

// rowFromColumns maps a column map to a SourceRow using the plan's column mapping.
func rowFromColumns(p *sourcePlan, cols map[string]any, ts time.Time) (objstore.SourceRow, error) {
	r := objstore.SourceRow{Props: map[string]any{}, TS: ts}
	for _, c := range p.columns {
		v, ok := cols[c.col]
		if !ok {
			continue // column not in the event (e.g. excluded by the connector): leave unchanged
		}
		nv, err := normalize(c.base, v)
		if err != nil {
			return r, fmt.Errorf("column %s: %w", c.col, err)
		}
		r.Props[c.prop] = nv
		if c.prop == p.ot.PrimaryKey && nv != nil {
			r.PK = fmt.Sprint(nv)
		}
	}
	if p.tsCol != "" {
		if v, ok := cols[p.tsCol]; ok {
			if t, err := normalize(spec.TTimestamp, v); err == nil && t != nil {
				r.TS = t.(time.Time)
			}
		}
	}
	if r.PK == "" {
		return r, fmt.Errorf("event has no primary key column %s", p.pkCol)
	}
	return r, nil
}

// ApplyEvent merges one change event into the object table it belongs to. Events for tables
// the ontology does not map are ignored (returns false).
func (ix *Indexer) ApplyEvent(ctx context.Context, ont *spec.CompiledOntology, connector string, ev *ChangeEvent) (bool, error) {
	for _, name := range sortedKeys(ont.ObjectTypes) {
		ot := ont.ObjectTypes[name]
		if ot.Datasource == nil || ot.Datasource.Connector != connector || !sameTable(ot.Datasource.Table, ev.Table) {
			continue
		}
		p, err := planFor(ot)
		if err != nil {
			return false, err
		}
		return true, pgx.BeginFunc(ctx, ix.DB, func(tx pgx.Tx) error {
			if ev.Op == "d" {
				r, err := rowFromColumns(p, ev.Before, ev.TS)
				if err != nil {
					return err
				}
				_, err = ix.Store.DeleteFromSource(ctx, tx, ont, name, []string{r.PK})
				return err
			}
			r, err := rowFromColumns(p, ev.After, ev.TS)
			if err != nil {
				return err
			}
			_, err = ix.Store.UpsertFromSource(ctx, tx, ont, name, []objstore.SourceRow{r})
			return err
		})
	}
	return false, nil
}

// sameTable compares "schema.table" names, treating a missing schema as public.
func sameTable(a, b string) bool {
	norm := func(s string) string {
		if !bytes.ContainsRune([]byte(s), '.') {
			return "public." + s
		}
		return s
	}
	return norm(a) == norm(b)
}
