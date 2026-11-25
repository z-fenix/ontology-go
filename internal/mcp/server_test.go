package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/spec"

	_ "github.com/acme/ontology/internal/expr"
)

func loadOntology(t *testing.T) *spec.CompiledOntology {
	t.Helper()
	files, ld := spec.LoadFS(os.DirFS("../../examples/ontology"), "examples/ontology")
	ont, cd := spec.Compile(files)
	if d := append(ld, cd...); d.HasErrors(false) {
		t.Fatalf("example: %v", d.Codes())
	}
	ont.Version = 3
	return ont
}

type fixedOnt struct{ o *spec.CompiledOntology }

func (f fixedOnt) Active(context.Context) (*spec.CompiledOntology, error) { return f.o, nil }

type call struct {
	Method, Path, Actor, Idem string
	Body                      map[string]any
}

// fakeBackend stands in for query-service and action-service.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []call
	failValid bool
}

func (f *fakeBackend) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("X-Actor"), r.Header.Get("Idempotency-Key"), body})
		failValid := f.failValid
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/validate") && failValid:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"errorCode":"SUBMISSION_CRITERIA_FAILED","message":"只有 OPEN 状态的工单可以指派"}`))
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_, _ = w.Write([]byte(`{"status":"VALID"}`))
		case strings.HasSuffix(r.URL.Path, "/apply"):
			_, _ = w.Write([]byte(`{"status":"COMMITTED","actionRid":"ri.action-run.1"}`))
		case strings.Contains(r.URL.Path, "/objects/WorkOrder/WO-404"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorCode":"OBJECT_NOT_FOUND","message":"object WorkOrder/WO-404 not found"}`))
		default:
			_, _ = w.Write([]byte(`{"type":"WorkOrder","pk":"WO-1","properties":{"status":"OPEN","estimatedCost":null},"__redacted":["estimatedCost"]}`))
		}
	})
}

func (f *fakeBackend) applies() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if strings.HasSuffix(c.Path, "/apply") {
			out = append(out, c)
		}
	}
	return out
}

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	backend *fakeBackend
	now     time.Time
	id      int
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, backend: &fakeBackend{}, now: time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)}
	be := httptest.NewServer(h.backend.handler())
	t.Cleanup(be.Close)
	s := &Server{
		Ontologies:     fixedOnt{loadOntology(t)},
		Backend:        &Backend{QueryURL: be.URL, ActionURL: be.URL, Ontology: "mro"},
		Auth:           auth.HeaderAuthenticator{},
		AllowedOrigins: []string{"https://app.example.com"},
		ConfirmSecret:  []byte("0123456789abcdef0123456789abcdef"),
		Now:            func() time.Time { return h.now },
	}
	h.srv = httptest.NewServer(s.Routes())
	t.Cleanup(h.srv.Close)
	return h
}

type rpcOut struct {
	Status int
	Result map[string]any
	Error  *rpcError
}

// send posts a JSON-RPC request with correct headers unless overridden in hdr ("" deletes).
func (h *harness) send(method string, params map[string]any, hdr map[string]string) rpcOut {
	h.t.Helper()
	h.id++
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["_meta"]; !ok {
		params["_meta"] = map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientInfo: map[string]any{"name": "test", "version": "1"},
			MetaClientCapabilities: map[string]any{"elicitation": map[string]any{}}}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": h.id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp", bytes.NewReader(body))
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
		"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": method, "X-Actor": "alice"}
	if n, ok := params["name"].(string); ok {
		headers["Mcp-Name"] = n
	}
	for k, v := range hdr {
		headers[k] = v
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return rpcOut{Status: resp.StatusCode, Result: out.Result, Error: out.Error}
}

func (h *harness) tool(name string, args map[string]any, extra map[string]any) rpcOut {
	p := map[string]any{"name": name, "arguments": args}
	for k, v := range extra {
		p[k] = v
	}
	return h.send("tools/call", p, nil)
}

func TestDiscover(t *testing.T) {
	h := newHarness(t)
	out := h.send("server/discover", nil, map[string]string{"X-Actor": ""}) // no auth needed
	if out.Status != 200 || out.Error != nil {
		t.Fatalf("discover = %+v", out)
	}
	if v := out.Result["supportedVersions"].([]any); len(v) != 1 || v[0] != ProtocolVersion {
		t.Errorf("supportedVersions = %v", v)
	}
	if out.Result["_meta"].(map[string]any)[MetaServerInfo] == nil {
		t.Error("serverInfo missing")
	}
}

func TestTransportValidation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name   string
		method string
		params map[string]any
		hdr    map[string]string
		status int
		code   int
	}{
		{"missing version header", "tools/list", nil, map[string]string{"MCP-Protocol-Version": ""}, 400, CodeHeaderMismatch},
		{"header/body mismatch", "tools/list", map[string]any{"_meta": map[string]any{MetaProtocolVersion: "2025-11-25"}}, nil, 400, CodeHeaderMismatch},
		{"unsupported version", "tools/list", map[string]any{"_meta": map[string]any{MetaProtocolVersion: "2025-11-25"}},
			map[string]string{"MCP-Protocol-Version": "2025-11-25"}, 400, CodeUnsupportedProtocolVersion},
		{"method header mismatch", "tools/list", nil, map[string]string{"Mcp-Method": "tools/call"}, 400, CodeHeaderMismatch},
		{"name header mismatch", "tools/call", map[string]any{"name": ToolDescribe}, map[string]string{"Mcp-Name": "objects_get"}, 400, CodeHeaderMismatch},
		{"unknown method", "resources/list", nil, nil, 404, CodeMethodNotFound},
		{"not authenticated", "tools/list", nil, map[string]string{"X-Actor": ""}, 401, CodeInvalidRequest},
		{"bad origin", "tools/list", nil, map[string]string{"Origin": "https://evil.example"}, 403, CodeInvalidRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := h.send(c.method, c.params, c.hdr)
			if out.Status != c.status || out.Error == nil || out.Error.Code != c.code {
				t.Fatalf("got %d %+v, want %d code %d", out.Status, out.Error, c.status, c.code)
			}
		})
	}
	if out := h.send("tools/list", nil, map[string]string{"Origin": "https://app.example.com"}); out.Status != 200 {
		t.Errorf("allowed origin: %d", out.Status)
	}
	enc := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(ToolDescribe)) + "?="
	if out := h.send("tools/call", map[string]any{"name": ToolDescribe}, map[string]string{"Mcp-Name": enc}); out.Status != 200 || out.Error != nil {
		t.Errorf("base64 Mcp-Name: %+v", out)
	}
	resp, _ := http.Get(h.srv.URL + "/mcp")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", resp.StatusCode)
	}
}

func TestToolsListIsGeneratedAndDeterministic(t *testing.T) {
	h := newHarness(t)
	out := h.send("tools/list", nil, nil)
	tools := out.Result["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	want := "ontology_describe,objects_get,objects_links,objects_search,objects_aggregate,action_validate,action_assignWorkOrder"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v (groundAircraft is not exposed to agents)", names)
	}
	assign := tools[len(tools)-1].(map[string]any)
	schema := assign["inputSchema"].(map[string]any)
	req := schema["required"].([]any)
	if len(req) != 2 || schema["additionalProperties"] != false {
		t.Errorf("assign schema = %v", schema)
	}
	if !strings.Contains(assign["description"].(string), "human must confirm") {
		t.Errorf("description should mention confirmation: %s", assign["description"])
	}
	if ann := assign["annotations"].(map[string]any); ann["readOnlyHint"] != false {
		t.Errorf("annotations = %v", ann)
	}
	again := h.send("tools/list", nil, nil)
	a, _ := json.Marshal(out.Result["tools"])
	b, _ := json.Marshal(again.Result["tools"])
	if string(a) != string(b) {
		t.Error("tools/list must be deterministic")
	}
}

func TestReadTools(t *testing.T) {
	h := newHarness(t)
	out := h.tool(ToolDescribe, map[string]any{"objectType": "WorkOrder"}, nil)
	sc := out.Result["structuredContent"].(map[string]any)
	if len(sc["objectTypes"].([]any)) != 1 || out.Result["isError"] != false {
		t.Fatalf("describe = %+v", out.Result)
	}
	text := out.Result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "→ aircraft: Aircraft (one)") || !strings.Contains(text, "[OPEN IN_PROGRESS DONE CANCELLED]") {
		t.Errorf("describe text:\n%s", text)
	}

	out = h.tool(ToolGet, map[string]any{"objectType": "WorkOrder", "pk": "WO-1"}, nil)
	if out.Result["isError"] != false || out.Result["structuredContent"].(map[string]any)["__redacted"] == nil {
		t.Errorf("get = %+v", out.Result)
	}
	out = h.tool(ToolGet, map[string]any{"objectType": "WorkOrder", "pk": "WO-404"}, nil)
	if out.Result["isError"] != true || !strings.Contains(out.Result["content"].([]any)[0].(map[string]any)["text"].(string), "OBJECT_NOT_FOUND") {
		t.Errorf("missing object must be a tool error: %+v", out.Result)
	}
	if out := h.tool(ToolGet, map[string]any{"objectType": "Nope", "pk": "x"}, nil); out.Error == nil || out.Error.Code != CodeInvalidParams {
		t.Errorf("unknown type: %+v", out)
	}

	h.tool(ToolSearch, map[string]any{"objectType": "WorkOrder", "pageSize": 500,
		"where": map[string]any{"eq": map[string]any{"field": "status", "value": "OPEN"}}}, nil)
	last := h.backend.calls[len(h.backend.calls)-1]
	if !strings.HasSuffix(last.Path, "/objects/WorkOrder/search") || last.Actor != "alice" {
		t.Errorf("search call = %+v", last)
	}
	if last.Body["pageSize"] != float64(100) || last.Body["objectType"] != nil || last.Body["where"] == nil {
		t.Errorf("search body = %v (pageSize capped, objectType stripped)", last.Body)
	}
}

func elicitationState(t *testing.T, out rpcOut) string {
	t.Helper()
	if out.Result["resultType"] != "input_required" {
		t.Fatalf("expected input_required, got %+v", out)
	}
	req := out.Result["inputRequests"].(map[string]any)[confirmKey].(map[string]any)
	if req["method"] != "elicitation/create" || !strings.Contains(req["params"].(map[string]any)["message"].(string), "workOrder = WO-1") {
		t.Fatalf("elicitation = %v", req)
	}
	return out.Result["requestState"].(string)
}

func accept(confirm bool) map[string]any {
	action := "accept"
	if !confirm {
		action = "decline"
	}
	return map[string]any{confirmKey: map[string]any{"action": action, "content": map[string]any{"confirm": confirm}}}
}

func TestActionRequiresHumanConfirmation(t *testing.T) {
	h := newHarness(t)
	args := map[string]any{"workOrder": "WO-1", "technician": "T-1", "note": "night shift"}
	first := h.tool("action_assignWorkOrder", args, nil)
	state := elicitationState(t, first)
	if len(h.backend.applies()) != 0 {
		t.Fatal("nothing may run before confirmation")
	}

	// Tampered arguments, wrong caller, or an expired state are refused.
	changed := map[string]any{"workOrder": "WO-2", "technician": "T-1", "note": "night shift"}
	if out := h.tool("action_assignWorkOrder", changed, map[string]any{"requestState": state, "inputResponses": accept(true)}); out.Result["isError"] != true {
		t.Errorf("changed arguments must be refused: %+v", out.Result)
	}
	if out := h.send("tools/call", map[string]any{"name": "action_assignWorkOrder", "arguments": args, "requestState": state,
		"inputResponses": accept(true)}, map[string]string{"X-Actor": "mallory"}); out.Result["isError"] != true {
		t.Errorf("another caller must be refused: %+v", out.Result)
	}

	// Declined: nothing runs.
	if out := h.tool("action_assignWorkOrder", args, map[string]any{"requestState": state, "inputResponses": accept(false)}); out.Result["isError"] != false ||
		out.Result["structuredContent"].(map[string]any)["status"] != "DECLINED" {
		t.Errorf("decline = %+v", out.Result)
	}
	if len(h.backend.applies()) != 0 {
		t.Fatal("a declined action must not run")
	}

	// Accepted: runs once, with an idempotency key derived from the confirmation.
	ok := h.tool("action_assignWorkOrder", args, map[string]any{"requestState": state, "inputResponses": accept(true)})
	if ok.Result["isError"] != false {
		t.Fatalf("accept = %+v", ok.Result)
	}
	h.tool("action_assignWorkOrder", args, map[string]any{"requestState": state, "inputResponses": accept(true)}) // client retry
	applies := h.backend.applies()
	if len(applies) != 2 || applies[0].Idem == "" || applies[0].Idem != applies[1].Idem || applies[0].Actor != "alice" {
		t.Fatalf("applies = %+v (a retry must reuse the idempotency key)", applies)
	}
	if params := applies[0].Body["parameters"].(map[string]any); params["workOrder"] != "WO-1" {
		t.Errorf("apply params = %v", params)
	}

	h.now = h.now.Add(ConfirmTTL + time.Minute)
	if out := h.tool("action_assignWorkOrder", args, map[string]any{"requestState": state, "inputResponses": accept(true)}); out.Result["isError"] != true {
		t.Errorf("an expired confirmation must be refused: %+v", out.Result)
	}
}

func TestConfirmationNotRequestedWhenValidationFails(t *testing.T) {
	h := newHarness(t)
	h.backend.failValid = true
	out := h.tool("action_assignWorkOrder", map[string]any{"workOrder": "WO-9", "technician": "T-1"}, nil)
	if out.Result["resultType"] != "complete" || out.Result["isError"] != true ||
		!strings.Contains(out.Result["content"].([]any)[0].(map[string]any)["text"].(string), "SUBMISSION_CRITERIA_FAILED") {
		t.Fatalf("result = %+v", out.Result)
	}
}

func TestUnexposedActionIsUnknown(t *testing.T) {
	h := newHarness(t)
	if out := h.tool("action_groundAircraft", map[string]any{"aircraft": "B-1", "reason": "x"}, nil); out.Error == nil || out.Error.Code != CodeInvalidParams {
		t.Fatalf("groundAircraft is not exposed: %+v", out)
	}
	if out := h.tool(ToolValidate, map[string]any{"action": "groundAircraft", "parameters": map[string]any{}}, nil); out.Error == nil {
		t.Fatal("validate must not reach unexposed actions")
	}
}

func TestConfirmationNeedsElicitationCapability(t *testing.T) {
	h := newHarness(t)
	params := map[string]any{"name": "action_assignWorkOrder", "arguments": map[string]any{"workOrder": "WO-1", "technician": "T-1"},
		"_meta": map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientCapabilities: map[string]any{}}}
	out := h.send("tools/call", params, nil)
	if out.Error == nil || out.Error.Code != CodeMissingClientCapability {
		t.Fatalf("expected MissingRequiredClientCapability, got %+v", out)
	}
	if len(h.backend.applies()) != 0 {
		t.Fatal("nothing may run")
	}
}
