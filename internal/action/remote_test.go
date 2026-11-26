package action

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acme/ontology/internal/objstore"
)

// fakeRuntime implements groundAircraft remotely: it reads open work orders through the callback
// and returns edits, like runtimes/python/example_functions.py.
func fakeRuntime(t *testing.T, mode string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/functions/groundAircraft" || r.Header.Get("Authorization") != "Bearer rt-token" {
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
			return
		}
		var req functionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		switch mode {
		case "reject":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"errorCode":"AIRCRAFT_IN_FLIGHT","message":"aircraft is airborne"}`))
			return
		case "crash":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		case "badvalue":
			_, _ = w.Write([]byte(`{"edits":[{"op":"MODIFY","type":"WorkOrder","pk":"WO-1","changes":{"priority":"high"}}]}`))
			return
		}
		ac := req.Objects["aircraft"]
		if ac == nil || req.Actor.ID != "alice" || req.Params["reason"] != "bird strike" {
			t.Errorf("request = %+v", req)
		}
		find, _ := json.Marshal(map[string]any{"type": "WorkOrder", "eq": map[string]any{"tailNumber": ac.PK, "status": "OPEN"}})
		resp, err := http.Post(req.Reader.URL+"/find", "application/json", bytes.NewReader(find))
		if err != nil {
			t.Errorf("callback: %v", err)
			return
		}
		defer resp.Body.Close()
		var found struct {
			Data []wireObject `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&found)
		edits := []map[string]any{{"op": "MODIFY", "type": "Aircraft", "pk": ac.PK, "expectedVersion": ac.Version,
			"changes": map[string]any{"status": "GROUNDED"}}}
		for _, wo := range found.Data {
			edits = append(edits, map[string]any{"op": "modify", "type": "WorkOrder", "pk": wo.PK,
				"expectedVersion": wo.Version, "changes": map[string]any{"priority": 1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"edits": edits})
	}))
}

func remoteEngine(t *testing.T, mode string) (*Engine, *fakeCommitter, *FunctionCallbacks) {
	t.Helper()
	e, c, _ := fixture(t)
	cb := NewFunctionCallbacks()
	self := httptest.NewServer(http.StripPrefix("/internal/function-reads", cb.Handler()))
	t.Cleanup(self.Close)
	rt := fakeRuntime(t, mode)
	t.Cleanup(rt.Close)
	ont := e.Ontologies.(fakeOnt).o
	ga := *ont.ActionTypes["groundAircraft"]
	fn := *ga.Function
	fn.Runtime = "python"
	ga.Function = &fn
	ont.ActionTypes["groundAircraft"] = &ga
	e.Runtimes = map[string]*RemoteRuntime{"python": {Name: "python", URL: rt.URL, CallbackURL: self.URL,
		Token: "rt-token", Timeout: 5 * time.Second, Callbacks: cb}}
	return e, c, cb
}

func ground() ApplyRequest {
	return ApplyRequest{ActionType: "groundAircraft", Actor: user, Mode: ModeApply,
		Parameters: map[string]any{"aircraft": "B-1234", "reason": "bird strike"}}
}

func TestRemoteFunction(t *testing.T) {
	e, c, cb := remoteEngine(t, "ok")
	res, err := e.Apply(context.Background(), ground())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edits.Modified) != 2 || len(c.commits) != 1 {
		t.Fatalf("result = %+v", res.Edits)
	}
	for _, ed := range c.commits[0].Edits {
		if ed.Ref.Type == "WorkOrder" && (ed.Op != objstore.OpModify || ed.Changes["priority"] != int64(1) || ed.ExpectedVersion != 1) {
			t.Errorf("work order edit = %+v (priority must be coerced to int64)", ed)
		}
	}
	if len(cb.live) != 0 {
		t.Error("the callback must be removed after the invocation")
	}
}

func TestRemoteFunctionErrors(t *testing.T) {
	for mode, want := range map[string]string{
		"reject":   "AIRCRAFT_IN_FLIGHT",
		"crash":    "FUNCTION_FAILED",
		"badvalue": "EDIT_VALIDATION_FAILED",
	} {
		e, c, _ := remoteEngine(t, mode)
		_, err := e.Apply(context.Background(), ground())
		if got := code(t, err); got != want {
			t.Errorf("%s: code = %s, want %s", mode, got, want)
		}
		if len(c.commits) != 0 {
			t.Errorf("%s: nothing may be committed", mode)
		}
	}
	e, _, _ := remoteEngine(t, "ok")
	e.Runtimes = nil
	if got := code(t, func() error { _, err := e.Apply(context.Background(), ground()); return err }()); got != "FUNCTION_RUNTIME_UNAVAILABLE" {
		t.Errorf("unconfigured runtime: code = %s", got)
	}
}

func TestCallbackRejectsUnknownToken(t *testing.T) {
	cb := NewFunctionCallbacks()
	rec := httptest.NewRecorder()
	cb.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nope/get", strings.NewReader(`{"type":"Aircraft","pk":"B-1234"}`)))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"INVOCATION_NOT_FOUND"`) {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// TestCallbackErrorCodes pins the error codes of the read callback.
func TestCallbackErrorCodes(t *testing.T) {
	ont := loadOntology(t)
	cb := NewFunctionCallbacks()
	token, err := cb.add(&invocation{ctx: context.Background(), ont: ont})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body, code string }{
		{"/" + token + "/find", `{"type":"WorkOrder","bogus":1}`, "INVALID_BODY"},
		{"/" + token + "/find", `{"type":"WorkOrder","eq":{"priority":"high"}}`, "INVALID_FILTER"},
		{"/" + token + "/find", `{"type":"Nope"}`, "UNKNOWN_OBJECT_TYPE"},
		{"/" + token + "/find", `{"type":"WorkOrder","eq":{"nope":1}}`, "UNKNOWN_PROPERTY"},
		{"/" + token + "/get", `{"type":"WorkOrder","pk":"x","extra":1}`, "INVALID_BODY"},
	} {
		rec := httptest.NewRecorder()
		cb.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s %s: status = %d, body = %s; want 400 %s", tc.path, tc.body, rec.Code, rec.Body, tc.code)
		}
	}
}

func TestDecodeEditsRejectsMalformed(t *testing.T) {
	ont := loadOntology(t)
	for _, raw := range []string{
		`[{"op":"UPSERT","type":"WorkOrder","pk":"x"}]`,
		`[{"op":"MODIFY","type":"","pk":"x"}]`,
		`[{"op":"LINK","type":"WorkOrder","pk":"x"}]`,
	} {
		var in []wireEdit
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeEdits(ont, in); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
}

func TestParseRuntimes(t *testing.T) {
	m, err := ParseRuntimes("python=http://py:9100, typescript=https://ts.internal")
	if err != nil || m["python"] != "http://py:9100" || m["typescript"] != "https://ts.internal" {
		t.Errorf("= %v, %v", m, err)
	}
	for _, bad := range []string{"go=http://x", "python", "python=ftp://x", "python=http://"} {
		if _, err := ParseRuntimes(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
