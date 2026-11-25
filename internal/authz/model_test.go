package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/acme/ontology/internal/spec"

	_ "github.com/acme/ontology/internal/expr"
)

func loadRepoModel(t *testing.T) (*Model, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/openfga/model.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseModel(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, raw
}

func exampleOnt(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example: %v", d.Codes())
	}
	return ont
}

func TestRepoModelMatchesExampleOntology(t *testing.T) {
	m, _ := loadRepoModel(t)
	if p := CheckModel(exampleOnt(t), m); len(p) != 0 {
		t.Fatalf("deploy/openfga/model.json and examples/ontology disagree: %v", p)
	}
}

func TestCheckModelFindsMismatches(t *testing.T) {
	m, _ := loadRepoModel(t)
	ont := exampleOnt(t)
	// Problems, in order: relation missing, wrong user type, computed relation not writable,
	// target without a fixed type, fgaType missing, action permission relation missing.
	ont.ObjectTypes["Aircraft"].Authz.Tuples = append(ont.ObjectTypes["Aircraft"].Authz.Tuples,
		spec.AuthzTuple{Relation: "owner", Target: "user:{{tailNumber}}"},
		spec.AuthzTuple{Relation: "base", Target: "user:{{baseAirport}}"},
		spec.AuthzTuple{Relation: "viewer", Target: "user:{{baseAirport}}"},
		spec.AuthzTuple{Relation: "base", Target: "{{baseAirport}}"})
	ont.ObjectTypes["Part"].Authz = &spec.ObjectAuthz{FGAType: "part"}
	ont.ActionTypes["groundAircraft"].Permission.Relation = "can_fly"
	got := CheckModel(ont, m)
	want := []string{`"owner" is not defined`, `does not accept "user"`, `cannot be written directly`,
		`must start with a fixed type`, `type "part" is not defined`, `"can_fly" is not defined`}
	joined := ""
	for _, p := range got {
		joined += p.String() + "\n"
	}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing problem %q in:\n%s", w, joined)
		}
	}
	if len(got) != len(want) {
		t.Errorf("problems = %d, want %d:\n%s", len(got), len(want), joined)
	}
}

type fakeFGA struct {
	mu     sync.Mutex
	stores []Store
	models map[string][]json.RawMessage
	writes int
}

func (f *fakeFGA) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/stores":
			_ = json.NewEncoder(w).Encode(map[string]any{"stores": f.stores})
		case r.Method == "POST" && r.URL.Path == "/stores":
			var b struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&b)
			s := Store{ID: "store-" + b.Name, Name: b.Name}
			f.stores = append(f.stores, s)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(s)
		case strings.HasSuffix(r.URL.Path, "/authorization-models") && r.Method == "GET":
			id := strings.Split(r.URL.Path, "/")[2]
			ms := f.models[id]
			if len(ms) > 0 {
				ms = ms[len(ms)-1:]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_models": ms})
		case strings.HasSuffix(r.URL.Path, "/authorization-models") && r.Method == "POST":
			id := strings.Split(r.URL.Path, "/")[2]
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mid := "model-" + string(rune('0'+len(f.models[id])+1))
			body["id"] = mid
			b, _ := json.Marshal(body)
			f.models[id] = append(f.models[id], b)
			f.writes++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"authorization_model_id": mid})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestEnsureStoreAndModelIdempotent(t *testing.T) {
	f := &fakeFGA{models: map[string][]json.RawMessage{}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, raw := loadRepoModel(t)
	a := &Admin{APIURL: srv.URL}
	ctx := context.Background()

	s, created, err := a.EnsureStore(ctx, "ontology")
	if err != nil || !created {
		t.Fatalf("first EnsureStore = %v %v", created, err)
	}
	id1, wrote, err := a.EnsureModel(ctx, s.ID, raw)
	if err != nil || !wrote || id1 == "" {
		t.Fatalf("first EnsureModel = %q %v %v", id1, wrote, err)
	}

	s2, created, _ := a.EnsureStore(ctx, "ontology")
	id2, wrote, err := a.EnsureModel(ctx, s2.ID, raw)
	if created || s2.ID != s.ID || wrote || id2 != id1 || err != nil {
		t.Fatalf("second run must change nothing: created=%v wrote=%v id=%q err=%v", created, wrote, id2, err)
	}

	var changed map[string]any
	_ = json.Unmarshal(raw, &changed)
	defs := changed["type_definitions"].([]any)
	changed["type_definitions"] = append(defs, map[string]any{"type": "extra"})
	b, _ := json.Marshal(changed)
	if id3, wrote, _ := a.EnsureModel(ctx, s.ID, b); !wrote || id3 == id1 {
		t.Fatalf("a changed model must be uploaded: wrote=%v id=%q", wrote, id3)
	}
	if f.writes != 2 {
		t.Errorf("model writes = %d, want 2", f.writes)
	}
}
