package objstore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func compile(t *testing.T, typ string, w *Where) (string, []any, []QueryField, error) {
	t.Helper()
	ont := loadExample(t)
	m, err := meta(ont, typ)
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{ont: ont}
	sql, err := b.cond(m, "t0", w, 0)
	return sql, b.args, b.fields, err
}

func mustContain(t *testing.T, sql string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(sql, p) {
			t.Errorf("SQL lacks %s\n%s", p, sql)
		}
	}
}

func TestCompileForeignKeyLink(t *testing.T) {
	sql, args, fields, err := compile(t, "WorkOrder", &Where{And: []Where{
		{Eq: &Cmp{Field: "status", Value: "OPEN"}},
		{Link: &LinkCmp{Path: "aircraft", Where: &Where{Eq: &Cmp{Field: "baseAirport", Value: "SIN"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sql, `"t0"."status" = $1`,
		`EXISTS (SELECT 1 FROM "obj"."aircraft" "t1" WHERE "t1"."_pk" = "t0"."tail_number"::text`,
		`"t1"."base_airport" = $2`, `NOT "t1"."_deleted"`)
	if !reflect.DeepEqual(args, []any{"OPEN", "SIN"}) {
		t.Errorf("args = %v", args)
	}
	want := []QueryField{{"WorkOrder", "status"}, {"Aircraft", "baseAirport"}}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("fields = %v", fields)
	}
}

func TestCompileReverseAndManyToManyLinks(t *testing.T) {
	sql, _, _, err := compile(t, "Aircraft", &Where{Link: &LinkCmp{Path: "workOrders",
		Where: &Where{Eq: &Cmp{Field: "status", Value: "DONE"}}}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sql, `FROM "obj"."work_order" "t1" WHERE "t1"."tail_number"::text = "t0"."_pk"`)

	sql, _, _, err = compile(t, "Aircraft", &Where{Link: &LinkCmp{Path: "parts",
		Where: &Where{Eq: &Cmp{Field: "partNumber", Value: "PN-1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sql, `FROM "obj"."link_aircraft_parts" "t2" JOIN "obj"."part" "t1" ON "t1"."_pk" = "t2"."to_pk"`,
		`WHERE "t2"."from_pk" = "t0"."_pk" AND NOT "t2"."_deleted"`)
}

func TestCompileOperators(t *testing.T) {
	sql, args, _, err := compile(t, "WorkOrder", &Where{And: []Where{
		{Contains: &Cmp{Field: "summary", Value: "50%_off"}},
		{StartsWith: &Cmp{Field: "workOrderId", Value: "WO-"}},
		{In: &InCmp{Field: "status", Values: []any{"OPEN", "DONE"}}},
		{Not: &Where{Eq: &Cmp{Field: "priority", Value: 1}}},
		{Eq: &Cmp{Field: "technicianId", Value: nil}},
		{Gte: &Cmp{Field: "dueAt", Value: "2026-11-25T00:00:00Z"}},
		{In: &InCmp{Field: "status", Values: []any{}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sql, `"t0"."summary" ILIKE $1 ESCAPE '\'`, `"t0"."work_order_id" LIKE $2 ESCAPE '\'`,
		`"t0"."status" IN ($3, $4)`, `NOT COALESCE(("t0"."priority" = $5), FALSE)`,
		`"t0"."technician_id" IS NULL`, `"t0"."due_at" >= $6::timestamptz`, `(FALSE)`)
	if args[0] != `%50\%\_off%` || args[1] != "WO-%" {
		t.Errorf("escaped patterns = %q, %q", args[0], args[1])
	}

	sql, args, _, err = compile(t, "Technician", &Where{Contains: &Cmp{Field: "certifications", Value: "A320"}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sql, `"t0"."certifications" @> $1::text::jsonb`)
	if args[0] != `["A320"]` {
		t.Errorf("array arg = %v", args[0])
	}
}

func TestCompileRejects(t *testing.T) {
	cases := map[string]*Where{
		"two operators":       {Eq: &Cmp{Field: "status", Value: "OPEN"}, Gt: &Cmp{Field: "priority", Value: 1}},
		"no operator":         {},
		"unknown field":       {Eq: &Cmp{Field: "nope", Value: 1}},
		"vector field":        {Eq: &Cmp{Field: "descriptionEmbedding", Value: 1}},
		"bad value type":      {Eq: &Cmp{Field: "priority", Value: "high"}},
		"range without value": {Gt: &Cmp{Field: "summary", Value: nil}},
		"unknown link":        {Link: &LinkCmp{Path: "engines"}},
		"null in list":        {In: &InCmp{Field: "status", Values: []any{nil}}},
		"contains on int":     {Contains: &Cmp{Field: "priority", Value: 1}},
		"startsWith on int":   {StartsWith: &Cmp{Field: "priority", Value: "1"}},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := compile(t, "WorkOrder", w); !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	deep := &Where{Link: &LinkCmp{Path: "aircraft", Where: &Where{Link: &LinkCmp{Path: "workOrders",
		Where: &Where{Link: &LinkCmp{Path: "aircraft", Where: &Where{Link: &LinkCmp{Path: "workOrders"}}}}}}}}
	if _, _, _, err := compile(t, "WorkOrder", deep); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("4-deep link: err = %v", err)
	}
}

func TestQueryFieldsIncludesExtras(t *testing.T) {
	f, err := QueryFields(loadExample(t), "WorkOrder", nil, []string{"estimatedCost"})
	if err != nil || len(f) != 1 || f[0] != (QueryField{"WorkOrder", "estimatedCost"}) {
		t.Fatalf("fields = %v, %v", f, err)
	}
	if _, err := QueryFields(loadExample(t), "WorkOrder", nil, []string{"bogus"}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown extra field: %v", err)
	}
}
