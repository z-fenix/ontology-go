//go:build integration

package objstore

import (
	"context"
	"reflect"
	"testing"
)

func seedQuery(t *testing.T) (context.Context, *Store, func(typ string, q SearchQuery) []string) {
	t.Helper()
	ctx, pool, ont, s := setup(t)
	create := func(typ, pk string, props map[string]any) Edit {
		return Edit{Op: OpCreate, Ref: ObjectRef{Type: typ, PK: pk}, Changes: props}
	}
	wo := func(pk, tail, status string, prio int, cost any) Edit {
		return create("WorkOrder", pk, map[string]any{"summary": "check " + pk, "status": status, "tailNumber": tail,
			"priority": prio, "estimatedCost": cost})
	}
	if _, err := apply(ctx, pool, s, ont,
		create("Aircraft", "B-1", map[string]any{"baseAirport": "SIN", "status": "ACTIVE"}),
		create("Aircraft", "B-2", map[string]any{"baseAirport": "PEK", "status": "ACTIVE"}),
		wo("WO-1", "B-1", "OPEN", 1, "100.50"), wo("WO-2", "B-1", "OPEN", 3, "200"),
		wo("WO-3", "B-2", "DONE", 2, "50.25"), wo("WO-4", "B-2", "OPEN", 5, nil),
		create("Technician", "T-1", map[string]any{"name": "Lee", "certifications": []string{"A320"}}),
		create("Technician", "T-2", map[string]any{"name": "Tan", "certifications": []string{"B737"}}),
		create("Part", "P-1", map[string]any{"partNumber": "PN-1"}),
		Edit{Op: OpLink, LinkType: "aircraftParts", Ref: ObjectRef{Type: "Aircraft", PK: "B-1"}, Target: ObjectRef{Type: "Part", PK: "P-1"}},
	); err != nil {
		t.Fatal(err)
	}
	pks := func(typ string, q SearchQuery) []string {
		t.Helper()
		objs, _, err := s.Search(ctx, ont, typ, q)
		if err != nil {
			t.Fatalf("search %s: %v", typ, err)
		}
		out := []string{}
		for _, o := range objs {
			out = append(out, o.Ref.PK)
		}
		return out
	}
	return ctx, s, pks
}

func TestSearchIntegration(t *testing.T) {
	_, _, pks := seedQuery(t)
	check := func(name string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("open at SIN", pks("WorkOrder", SearchQuery{Where: &Where{And: []Where{
		{Eq: &Cmp{Field: "status", Value: "OPEN"}},
		{Link: &LinkCmp{Path: "aircraft", Where: &Where{Eq: &Cmp{Field: "baseAirport", Value: "SIN"}}}},
	}}}), []string{"WO-1", "WO-2"})
	check("priority desc", pks("WorkOrder", SearchQuery{OrderBy: []OrderBy{{Field: "priority", Direction: "desc"}}}),
		[]string{"WO-4", "WO-2", "WO-3", "WO-1"})
	check("page 2", pks("WorkOrder", SearchQuery{OrderBy: []OrderBy{{Field: "priority", Direction: "desc"}}, Offset: 2, Limit: 2}),
		[]string{"WO-3", "WO-1"})
	check("gt + isNull", pks("WorkOrder", SearchQuery{Where: &Where{And: []Where{
		{Gt: &Cmp{Field: "priority", Value: 2}}, {IsNull: &FieldRef{Field: "estimatedCost"}}}}}), []string{"WO-4"})
	check("not open", pks("WorkOrder", SearchQuery{Where: &Where{Not: &Where{Eq: &Cmp{Field: "status", Value: "OPEN"}}}}),
		[]string{"WO-3"})
	check("decimal range", pks("WorkOrder", SearchQuery{Where: &Where{Gte: &Cmp{Field: "estimatedCost", Value: "100.50"}}}),
		[]string{"WO-1", "WO-2"})
	check("array contains", pks("Technician", SearchQuery{Where: &Where{Contains: &Cmp{Field: "certifications", Value: "A320"}}}),
		[]string{"T-1"})
	check("m:n link", pks("Aircraft", SearchQuery{Where: &Where{Link: &LinkCmp{Path: "parts",
		Where: &Where{Eq: &Cmp{Field: "partNumber", Value: "PN-1"}}}}}), []string{"B-1"})
	check("reverse fk link", pks("Aircraft", SearchQuery{Where: &Where{Link: &LinkCmp{Path: "workOrders",
		Where: &Where{Eq: &Cmp{Field: "status", Value: "DONE"}}}}}), []string{"B-2"})
}

func TestAggregateIntegration(t *testing.T) {
	ctx, s, _ := seedQuery(t)
	ont := loadExample(t)
	rows, more, err := s.Aggregate(ctx, ont, "WorkOrder", AggQuery{GroupBy: []string{"tailNumber"},
		Metrics: []Metric{{Op: "count"}, {Op: "sum", Field: "estimatedCost"}, {Op: "max", Field: "priority"}, {Op: "avg", Field: "priority"}}})
	if err != nil || more || len(rows) != 2 {
		t.Fatalf("rows = %+v more=%v err=%v", rows, more, err)
	}
	b1, b2 := rows[0], rows[1]
	if b1.Group["tailNumber"] != "B-1" || b1.Metrics["count"] != int64(2) || b1.Metrics["sum_estimatedCost"] != "300.50" ||
		b1.Metrics["max_priority"] != int64(3) || b1.Metrics["avg_priority"] != 2.0 {
		t.Errorf("B-1 = %+v", b1)
	}
	if b2.Metrics["sum_estimatedCost"] != "50.25" || b2.Metrics["max_priority"] != int64(5) {
		t.Errorf("B-2 = %+v", b2)
	}
	rows, _, err = s.Aggregate(ctx, ont, "WorkOrder", AggQuery{RestrictPKs: []string{"WO-1", "WO-3"},
		Where: &Where{Eq: &Cmp{Field: "status", Value: "OPEN"}}})
	if err != nil || len(rows) != 1 || rows[0].Metrics["count"] != int64(1) {
		t.Errorf("restricted count = %+v, %v", rows, err)
	}
	rows, _, _ = s.Aggregate(ctx, ont, "WorkOrder", AggQuery{RestrictPKs: []string{}})
	if rows[0].Metrics["count"] != int64(0) {
		t.Errorf("empty restriction must count nothing: %+v", rows)
	}
}
