package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/acme/ontology/internal/spec"
)

func woType() *spec.ObjectType {
	return &spec.ObjectType{APIName: "WorkOrder", Authz: &spec.ObjectAuthz{FGAType: "work_order", Tuples: []spec.AuthzTuple{
		{Relation: "aircraft", Target: "aircraft:{{tailNumber}}"},
		{Relation: "assignee", Target: "user:{{technicianId}}"},
	}}}
}

func TestDesiredTuples(t *testing.T) {
	got := DesiredTuples(woType(), "WO-1", map[string]any{"tailNumber": "B-1", "technicianId": nil}, false)
	want := []Tuple{{User: "aircraft:B-1", Relation: "aircraft", Object: "work_order:WO-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tuples = %v", got)
	}
	if got := DesiredTuples(woType(), "WO-1", map[string]any{"tailNumber": "B-1"}, true); got != nil {
		t.Errorf("deleted object must have no tuples: %v", got)
	}
}

func TestDiffTuples(t *testing.T) {
	a := Tuple{"aircraft:B-1", "aircraft", "work_order:WO-1"}
	b := Tuple{"user:T-1", "assignee", "work_order:WO-1"}
	c := Tuple{"user:T-2", "assignee", "work_order:WO-1"}
	w, d := DiffTuples([]Tuple{a, b}, []Tuple{a, c}) // reassigned from T-1 to T-2
	if !reflect.DeepEqual(w, []Tuple{c}) || !reflect.DeepEqual(d, []Tuple{b}) {
		t.Errorf("writes=%v deletes=%v", w, d)
	}
	if w, d := DiffTuples([]Tuple{a}, []Tuple{a}); len(w)+len(d) != 0 {
		t.Error("no-op diff must be empty")
	}
}

func TestOpenFGAWriteBatching(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stores/s1/write" {
			http.NotFound(w, r)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	f := NewOpenFGA(srv.URL, "s1", "m1")
	writes := make([]Tuple, 150)
	for i := range writes {
		writes[i] = Tuple{User: "user:u", Relation: "viewer", Object: "doc:" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	if err := f.WriteTuples(context.Background(), writes, []Tuple{{"user:x", "viewer", "doc:z"}}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (100-tuple limit)", len(bodies))
	}
	first := bodies[0]
	if first["authorization_model_id"] != "m1" || first["deletes"] == nil {
		t.Errorf("first request = %v", first)
	}
	if w := first["writes"].(map[string]any); w["on_duplicate"] != "ignore" || len(w["tuple_keys"].([]any)) != 99 {
		t.Errorf("first writes = %v", w)
	}
}
