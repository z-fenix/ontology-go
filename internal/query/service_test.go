package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/spec"

	_ "github.com/acme/ontology/internal/expr"
)

func loadOntology(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example ontology: %v", d.Codes())
	}
	ont.Version = 1
	return ont
}

type fakeOnt struct{ o *spec.CompiledOntology }

func (f fakeOnt) Active(context.Context) (*spec.CompiledOntology, error)     { return f.o, nil }
func (f fakeOnt) Get(context.Context, int64) (*spec.CompiledOntology, error) { return f.o, nil }

// fakeStore keeps objects in PK order; Search ignores the filter (the store's SQL is tested in objstore).
type fakeStore struct{ objs []*objstore.Object }

func (f *fakeStore) ofType(typ string) []*objstore.Object {
	var out []*objstore.Object
	for _, o := range f.objs {
		if o.Ref.Type == typ {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.PK < out[j].Ref.PK })
	return out
}

func (f *fakeStore) GetMany(_ context.Context, _ *spec.CompiledOntology, refs []objstore.ObjectRef) (map[objstore.ObjectRef]*objstore.Object, error) {
	out := map[objstore.ObjectRef]*objstore.Object{}
	for _, r := range refs {
		for _, o := range f.objs {
			if o.Ref == r {
				out[r] = o
			}
		}
	}
	return out, nil
}

func (f *fakeStore) Links(_ context.Context, _ *spec.CompiledOntology, from objstore.ObjectRef, nav string, _ int) ([]*objstore.Object, error) {
	if nav != "workOrders" {
		return nil, objstore.ErrUnknownType
	}
	var out []*objstore.Object
	for _, o := range f.ofType("WorkOrder") {
		if o.Props["tailNumber"] == from.PK {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeStore) Search(_ context.Context, _ *spec.CompiledOntology, typ string, q objstore.SearchQuery) ([]*objstore.Object, bool, error) {
	all := f.ofType(typ)
	if q.Offset >= len(all) {
		return nil, false, nil
	}
	end := q.Offset + q.Limit
	more := end < len(all)
	if !more {
		end = len(all)
	}
	return all[q.Offset:end], more, nil
}

func (f *fakeStore) Aggregate(_ context.Context, _ *spec.CompiledOntology, _ string, q objstore.AggQuery) ([]objstore.AggRow, bool, error) {
	n := int64(len(q.RestrictPKs))
	return []objstore.AggRow{{Metrics: map[string]any{"count": n, "restrict": strings.Join(q.RestrictPKs, ",")}}}, false, nil
}

type denyAuthz struct {
	deny map[string]bool // "user|object"
	err  error
}

func (d denyAuthz) Check(_ context.Context, user, _, object string) (bool, error) {
	if d.err != nil {
		return false, d.err
	}
	return !d.deny[user+"|"+object], nil
}

func wo(pk, tail string, cost string) *objstore.Object {
	return &objstore.Object{Ref: objstore.ObjectRef{Type: "WorkOrder", PK: pk}, Version: 1,
		Props: map[string]any{"workOrderId": pk, "summary": "s", "status": "OPEN", "tailNumber": tail, "estimatedCost": cost}}
}

func fixture(t *testing.T) (*Service, *fakeStore) {
	st := &fakeStore{}
	for i, pk := range []string{"WO-1", "WO-2", "WO-3", "WO-4", "WO-5"} {
		st.objs = append(st.objs, wo(pk, []string{"B-1", "B-2"}[i%2], "100.00"))
	}
	st.objs = append(st.objs, &objstore.Object{Ref: objstore.ObjectRef{Type: "Aircraft", PK: "B-1"}, Props: map[string]any{"tailNumber": "B-1"}})
	clear, err := authz.ParseClearances("finance=CONFIDENTIAL")
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Ontologies: fakeOnt{loadOntology(t)}, Store: st, Authz: denyAuthz{}, Clearances: clear}, st
}

var alice = Caller{ID: "alice", Kind: "USER"}

func code(t *testing.T, err error) string {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperr.Error, got %v", err)
	}
	return ae.Code
}

func TestGetRedactsAboveClearance(t *testing.T) { // E16
	s, _ := fixture(t)
	v, _, err := s.Get(context.Background(), alice, nil, "WorkOrder", "WO-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Properties["estimatedCost"] != nil || len(v.Redacted) != 1 || v.Redacted[0] != "estimatedCost" || v.Properties["summary"] != "s" {
		t.Errorf("view = %+v", v)
	}
	fin := Caller{ID: "bob", Kind: "USER", Groups: []string{"finance"}}
	v, _, _ = s.Get(context.Background(), fin, nil, "WorkOrder", "WO-1")
	if v.Properties["estimatedCost"] != "100.00" || len(v.Redacted) != 0 {
		t.Errorf("finance view = %+v", v)
	}
}

func TestInvisibleLooksMissing(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{deny: map[string]bool{"user:alice|work_order:WO-1": true}}
	if _, _, err := s.Get(context.Background(), alice, nil, "WorkOrder", "WO-1"); code(t, err) != "OBJECT_NOT_FOUND" {
		t.Fatal("invisible object must look missing")
	}
	if _, _, err := s.Get(context.Background(), alice, nil, "WorkOrder", "WO-404"); code(t, err) != "OBJECT_NOT_FOUND" {
		t.Fatal("missing object")
	}
}

func TestAgentNeedsBothPermissions(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{deny: map[string]bool{"user:bob|work_order:WO-1": true}}
	agent := Caller{ID: "agent-1", Kind: "AGENT", OnBehalfOf: "bob"}
	if _, _, err := s.Get(context.Background(), agent, nil, "WorkOrder", "WO-1"); code(t, err) != "OBJECT_NOT_FOUND" {
		t.Fatal("agent acting for a user without access must not see the object")
	}
	if _, _, err := s.Get(context.Background(), Caller{ID: "agent-1", Kind: "AGENT"}, nil, "WorkOrder", "WO-2"); code(t, err) != "OBJECT_NOT_FOUND" {
		t.Fatal("agent without on-behalf-of must not see protected objects")
	}
}

func TestAuthzOutageFailsClosed(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{err: errors.New("down")}
	if _, _, err := s.Get(context.Background(), alice, nil, "WorkOrder", "WO-1"); code(t, err) != "AUTHZ_UNAVAILABLE" {
		t.Fatal("outage must fail closed")
	}
}

func TestSearchPaginatesOverVisibleObjects(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{deny: map[string]bool{"user:alice|work_order:WO-2": true, "user:alice|work_order:WO-4": true}}
	var got []string
	tok := ""
	for i := 0; i < 5; i++ {
		page, err := s.Search(context.Background(), alice, "WorkOrder", SearchRequest{PageSize: 2, PageToken: tok})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Data {
			got = append(got, o.PK)
		}
		if tok = page.NextPageToken; tok == "" {
			break
		}
	}
	if strings.Join(got, ",") != "WO-1,WO-3,WO-5" {
		t.Errorf("visible pages = %v", got)
	}
}

func TestPageTokenBoundToQuery(t *testing.T) {
	s, _ := fixture(t)
	page, err := s.Search(context.Background(), alice, "WorkOrder", SearchRequest{PageSize: 2})
	if err != nil || page.NextPageToken == "" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	other := SearchRequest{PageSize: 2, PageToken: page.NextPageToken,
		Where: &objstore.Where{Eq: &objstore.Cmp{Field: "status", Value: "DONE"}}}
	if _, err := s.Search(context.Background(), alice, "WorkOrder", other); code(t, err) != "INVALID_PAGE_TOKEN" {
		t.Fatal("a token must not be reusable with a different filter")
	}
	if _, err := s.Search(context.Background(), alice, "WorkOrder", SearchRequest{PageToken: "garbage!"}); code(t, err) != "INVALID_PAGE_TOKEN" {
		t.Fatal("garbage token")
	}
}

func TestFilterOnRestrictedFieldForbidden(t *testing.T) {
	s, _ := fixture(t)
	req := SearchRequest{Where: &objstore.Where{Gt: &objstore.Cmp{Field: "estimatedCost", Value: "50"}}}
	if _, err := s.Search(context.Background(), alice, "WorkOrder", req); code(t, err) != "FIELD_NOT_PERMITTED" {
		t.Fatal("filtering on a redacted property must be refused")
	}
	sortReq := SearchRequest{OrderBy: []objstore.OrderBy{{Field: "estimatedCost"}}}
	if _, err := s.Search(context.Background(), alice, "WorkOrder", sortReq); code(t, err) != "FIELD_NOT_PERMITTED" {
		t.Fatal("sorting on a redacted property must be refused")
	}
	agg := AggregateRequest{Metrics: []objstore.Metric{{Op: "sum", Field: "estimatedCost"}}}
	if _, err := s.Aggregate(context.Background(), alice, "WorkOrder", agg); code(t, err) != "FIELD_NOT_PERMITTED" {
		t.Fatal("aggregating a redacted property must be refused")
	}
	fin := Caller{ID: "bob", Groups: []string{"finance"}}
	if _, err := s.Search(context.Background(), fin, "WorkOrder", req); err != nil {
		t.Fatalf("finance may filter: %v", err)
	}
	if _, err := s.Search(context.Background(), alice, "WorkOrder", SearchRequest{Where: &objstore.Where{Eq: &objstore.Cmp{Field: "nope", Value: 1}}}); code(t, err) != "INVALID_QUERY" {
		t.Fatal("unknown field")
	}
}

func TestAggregateRestrictedToVisible(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{deny: map[string]bool{"user:alice|work_order:WO-3": true}}
	res, err := s.Aggregate(context.Background(), alice, "WorkOrder", AggregateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups[0].Metrics["count"] != int64(4) || res.Groups[0].Metrics["restrict"] != "WO-1,WO-2,WO-4,WO-5" {
		t.Errorf("aggregate = %+v", res.Groups[0].Metrics)
	}
}

func TestLinksFilterInvisible(t *testing.T) {
	s, _ := fixture(t)
	s.Authz = denyAuthz{deny: map[string]bool{"user:alice|work_order:WO-3": true}}
	page, err := s.Links(context.Background(), alice, nil, "Aircraft", "B-1", "workOrders", 10)
	if err != nil {
		t.Fatal(err)
	}
	var pks []string
	for _, o := range page.Data {
		pks = append(pks, o.PK)
	}
	if strings.Join(pks, ",") != "WO-1,WO-5" {
		t.Errorf("links = %v", pks)
	}
}

func TestHandlerEndToEnd(t *testing.T) {
	s, _ := fixture(t)
	h := &Handler{Service: s, Ontology: "mro", Auth: auth.HeaderAuthenticator{}}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	do := func(method, path, actor, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if actor != "" {
			req.Header.Set("X-Actor", actor)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	if resp, _ := do("GET", "/api/v1/ontologies/mro/objects/WorkOrder/WO-1", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no identity: %d", resp.StatusCode)
	}
	resp, body := do("GET", "/api/v1/ontologies/mro/objects/WorkOrder/WO-1", "alice", "")
	if resp.StatusCode != 200 || resp.Header.Get("X-Ontology-Version") != "1" || body["__redacted"] == nil {
		t.Errorf("get = %d %v", resp.StatusCode, body)
	}
	resp, body = do("POST", "/api/v1/ontologies/mro/objects/WorkOrder/search", "alice", `{"pageSize":2}`)
	if resp.StatusCode != 200 || len(body["data"].([]any)) != 2 || body["nextPageToken"] == nil {
		t.Errorf("search = %d %v", resp.StatusCode, body)
	}
	if resp, _ := do("POST", "/api/v1/ontologies/mro/objects/WorkOrder/search", "alice", `{"bogus":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown body field: %d", resp.StatusCode)
	}
	if resp, _ := do("POST", "/api/v1/ontologies/mro/objects/WorkOrder/aggregate", "alice", ``); resp.StatusCode != 200 {
		t.Errorf("aggregate with empty body: %d", resp.StatusCode)
	}
	if resp, _ := do("GET", "/api/v1/ontologies/other/objects/WorkOrder/WO-1", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other ontology: %d", resp.StatusCode)
	}
}
