package indexer

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acme/ontology/internal/spec"
)

func loadOntology(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example ontology: %v", d.Codes())
	}
	return ont
}

func TestPlanUsesSourceColumns(t *testing.T) {
	ont := loadOntology(t)
	p, err := planFor(ont.ObjectTypes["Aircraft"])
	if err != nil {
		t.Fatal(err)
	}
	if p.pkCol != "tail_no" || p.table != "public.aircraft" {
		t.Errorf("pk=%s table=%s", p.pkCol, p.table)
	}
	sel := p.selectList()
	for _, want := range []string{`"tail_no"`, `"total_fh"::double precision`, `"base_iata"`} {
		if !strings.Contains(sel, want) {
			t.Errorf("select list %q lacks %s", sel, want)
		}
	}
	wo, _ := planFor(ont.ObjectTypes["WorkOrder"])
	if wo.tsCol != "updated_at" || !strings.Contains(wo.selectList(), `"estimated_cost"::text`) {
		t.Errorf("work order plan: ts=%s select=%s", wo.tsCol, wo.selectList())
	}
	if strings.Contains(wo.selectList(), "description_embedding") {
		t.Error("derived vector properties must not be read from the source")
	}
}

func TestNormalize(t *testing.T) {
	ok := []struct {
		base spec.BaseType
		in   any
		want any
	}{
		{spec.TString, []byte("x"), "x"},
		{spec.TInteger, "42", int64(42)},
		{spec.TLong, float64(7), int64(7)},
		{spec.TDouble, "1.5", 1.5},
		{spec.TDecimal, 12.5, "12.5"},
		{spec.TBoolean, "true", true},
		{spec.TDate, int64(20416), "2025-11-24"},
		{spec.TDate, "2026-11-25T00:00:00Z", "2026-11-25"},
		{spec.TString, nil, nil},
	}
	for _, c := range ok {
		got, err := normalize(c.base, c.in)
		if err != nil || got != c.want {
			t.Errorf("normalize(%s, %#v) = %#v, %v; want %#v", c.base, c.in, got, err, c.want)
		}
	}
	got, err := normalize(spec.TTimestamp, "2026-11-25T03:00:00+08:00")
	if err != nil || !got.(time.Time).Equal(time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp = %v, %v", got, err)
	}
	if _, err := normalize(spec.TInteger, "x"); err == nil {
		t.Error("expected an error for a non-integer")
	}
}

func TestParseDebezium(t *testing.T) {
	enveloped := `{"schema":{},"payload":{"op":"u","before":{"tail_no":"B-1234","status":"ACTIVE"},
"after":{"tail_no":"B-1234","status":"GROUNDED","total_fh":1234.5},
"source":{"schema":"public","table":"aircraft","ts_ms":1764039600000},"ts_ms":1764039600123}}`
	ev, err := ParseDebezium([]byte(enveloped))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Op != "u" || ev.Table != "public.aircraft" || ev.After["status"] != "GROUNDED" {
		t.Fatalf("event = %+v", ev)
	}
	if !ev.TS.Equal(time.UnixMilli(1764039600000).UTC()) {
		t.Errorf("ts = %v", ev.TS)
	}
	bare, err := ParseDebezium([]byte(`{"op":"d","before":{"tail_no":"B-9"},"after":null,"source":{"table":"aircraft"}}`))
	if err != nil || bare.Op != "d" || bare.Before["tail_no"] != "B-9" || bare.Table != "aircraft" {
		t.Fatalf("bare = %+v, %v", bare, err)
	}
	if _, err := ParseDebezium([]byte("null")); !errors.Is(err, ErrTombstone) {
		t.Errorf("tombstone: %v", err)
	}
	if _, err := ParseDebezium([]byte(`{"op":"x"}`)); err == nil {
		t.Error("unknown op must fail")
	}
}

func TestRowFromColumns(t *testing.T) {
	ont := loadOntology(t)
	p, _ := planFor(ont.ObjectTypes["WorkOrder"])
	ev, err := ParseDebezium([]byte(`{"op":"c","after":{"work_order_id":"WO-7","summary":"s","status":"OPEN",
"tail_number":"B-1234","priority":2,"estimated_cost":"99.90","updated_at":"2026-11-25T01:00:00Z"},"source":{"table":"work_orders"}}`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := rowFromColumns(p, ev.After, ev.TS)
	if err != nil {
		t.Fatal(err)
	}
	if r.PK != "WO-7" || r.Props["priority"] != int64(2) || r.Props["estimatedCost"] != "99.90" {
		t.Errorf("row = %+v", r)
	}
	if !r.TS.Equal(time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("row ts = %v (should come from updated_at)", r.TS)
	}
	if _, err := rowFromColumns(p, map[string]any{"summary": "no pk"}, ev.TS); err == nil {
		t.Error("missing primary key must fail")
	}
	if !sameTable("public.work_orders", "work_orders") || sameTable("audit.work_orders", "work_orders") {
		t.Error("sameTable is wrong")
	}
}
