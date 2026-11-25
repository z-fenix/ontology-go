package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/spec"
)

// OntologySource returns the active compiled ontology (satisfied by *omsclient.Cache).
type OntologySource interface {
	Active(ctx context.Context) (*spec.CompiledOntology, error)
}

// Server is an MCP endpoint over stateless Streamable HTTP (revision 2026-07-28).
type Server struct {
	Ontologies OntologySource
	Backend    *Backend
	// Auth identifies the caller; the backends re-verify the forwarded credentials.
	Auth auth.Authenticator
	// AllowedOrigins lists browser origins that may call the endpoint; requests without an
	// Origin header (non-browser clients) are always accepted. Spec: Origin MUST be validated.
	AllowedOrigins []string
	// ConfirmSecret signs pending human confirmations (32+ random bytes).
	ConfirmSecret []byte
	Name, Version string
	Now           func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Routes mounts the MCP endpoint at /mcp.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/mcp", s.serve)
	return mux
}

func writeRPC(w http.ResponseWriter, status int, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func rpcErr(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	writeRPC(w, status, rpcResponse{ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}})
}

func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, o := range s.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(o, "/"), strings.TrimRight(origin, "/")) {
			return true
		}
	}
	return false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r.Header.Get("Origin")) {
		rpcErr(w, http.StatusForbidden, nil, CodeInvalidRequest, "origin not allowed", nil)
		return
	}
	if r.Method != http.MethodPost {
		// Revision 2026-07-28 has no GET stream and no sessions (DELETE).
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20)); err != nil {
		rpcErr(w, http.StatusBadRequest, nil, CodeParseError, "cannot read body", nil)
		return
	}
	var req rpcRequest
	dec := json.NewDecoder(&buf)
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		rpcErr(w, http.StatusBadRequest, nil, CodeParseError, "invalid JSON", nil)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		rpcErr(w, http.StatusBadRequest, req.ID, CodeInvalidRequest, "not a JSON-RPC 2.0 request", nil)
		return
	}
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted) // this revision defines no client notifications over HTTP
		return
	}
	var bp baseParams
	if len(req.Params) > 0 {
		d := json.NewDecoder(bytes.NewReader(req.Params))
		d.UseNumber()
		if err := d.Decode(&bp); err != nil {
			rpcErr(w, http.StatusBadRequest, req.ID, CodeInvalidParams, "params must be an object", nil)
			return
		}
	}
	if status, code, msg, data := s.checkHeaders(r, &req, &bp); code != 0 {
		rpcErr(w, status, req.ID, code, msg, data)
		return
	}
	switch req.Method {
	case "server/discover":
		s.reply(w, req.ID, s.discover())
		return
	}
	p, err := s.Auth.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ontology-mcp"`)
		rpcErr(w, http.StatusUnauthorized, req.ID, CodeInvalidRequest, "authentication required", nil)
		return
	}
	ont, err := s.Ontologies.Active(r.Context())
	if err != nil {
		rpcErr(w, http.StatusServiceUnavailable, req.ID, CodeInternal, "ontology unavailable", nil)
		return
	}
	switch req.Method {
	case "tools/list":
		s.reply(w, req.ID, map[string]any{"resultType": "complete", "tools": BuildTools(ont),
			"ttlMs": 60000, "cacheScope": "private"})
	case "tools/call":
		var cp struct {
			Name           string                     `json:"name"`
			Arguments      map[string]any             `json:"arguments"`
			InputResponses map[string]json.RawMessage `json:"inputResponses"`
			RequestState   string                     `json:"requestState"`
		}
		d := json.NewDecoder(bytes.NewReader(req.Params))
		d.UseNumber()
		if err := d.Decode(&cp); err != nil || cp.Name == "" {
			rpcErr(w, http.StatusBadRequest, req.ID, CodeInvalidParams, "tools/call needs a name", nil)
			return
		}
		if cp.Arguments == nil {
			cp.Arguments = map[string]any{}
		}
		res, perr := s.callTool(r, ont, p, cp.Name, cp.Arguments, cp.InputResponses, cp.RequestState, bp.Meta.ClientCapabilities)
		if perr != nil {
			rpcErr(w, http.StatusOK, req.ID, perr.Code, perr.Message, perr.Data)
			return
		}
		s.reply(w, req.ID, res)
	default:
		rpcErr(w, http.StatusNotFound, req.ID, CodeMethodNotFound, "method not found: "+req.Method, nil)
	}
}

// checkHeaders enforces the request metadata rules of the Streamable HTTP transport.
func (s *Server) checkHeaders(r *http.Request, req *rpcRequest, bp *baseParams) (int, int, string, any) {
	mismatch := func(format string, args ...any) (int, int, string, any) {
		return http.StatusBadRequest, CodeHeaderMismatch, "Header mismatch: " + fmt.Sprintf(format, args...), nil
	}
	hv := r.Header.Get("MCP-Protocol-Version")
	if hv == "" {
		return mismatch("MCP-Protocol-Version header is required")
	}
	if bp.Meta == nil || bp.Meta.ProtocolVersion == "" {
		return http.StatusBadRequest, CodeInvalidParams, "params._meta." + MetaProtocolVersion + " is required", nil
	}
	if hv != bp.Meta.ProtocolVersion {
		return mismatch("MCP-Protocol-Version header %q does not match body %q", hv, bp.Meta.ProtocolVersion)
	}
	if hv != ProtocolVersion {
		return http.StatusBadRequest, CodeUnsupportedProtocolVersion, "Unsupported protocol version",
			map[string]any{"supported": []string{ProtocolVersion}, "requested": hv}
	}
	if m := r.Header.Get("Mcp-Method"); m != req.Method {
		return mismatch("Mcp-Method header %q does not match method %q", m, req.Method)
	}
	switch req.Method {
	case "tools/call", "resources/read", "prompts/get":
		want := bp.Name
		if req.Method == "resources/read" {
			want = bp.URI
		}
		got, ok := decodeHeaderValue(r.Header.Get("Mcp-Name"))
		if !ok || got != want {
			return mismatch("Mcp-Name header does not match %q", want)
		}
	}
	return 0, 0, "", nil
}

func (s *Server) serverInfo() map[string]any {
	name, ver := s.Name, s.Version
	if name == "" {
		name = "ontology-mcp"
	}
	if ver == "" {
		ver = "dev"
	}
	return map[string]any{"name": name, "version": ver}
}

func (s *Server) discover() map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": []string{ProtocolVersion},
		"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
		"instructions": "Read business objects and run governed actions of the ontology. Call ontology_describe first. " +
			"Every call runs with your user's permissions; hidden properties appear in __redacted. " +
			"Validate an action before running it; some actions need a human confirmation.",
		"ttlMs": 300000, "cacheScope": "public",
	}
}

func (s *Server) reply(w http.ResponseWriter, id json.RawMessage, result map[string]any) {
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[MetaServerInfo] = s.serverInfo()
	result["_meta"] = meta
	writeRPC(w, http.StatusOK, rpcResponse{ID: id, Result: result})
}

// ---------- tools ----------

type protoErr struct {
	Code    int
	Message string
	Data    any
}

func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case float64:
		return int(v)
	}
	return 0
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// backendResult turns a backend answer or error into a tool result. Business failures
// (permission denied, precondition failed, not found) are tool errors the agent can read and act on.
func backendResult(out map[string]any, err error) map[string]any {
	if err == nil {
		return textResult(pretty(out), out, false)
	}
	var be *BackendError
	if errors.As(err, &be) {
		detail := map[string]any{"errorCode": be.Code, "status": be.Status, "message": be.Message}
		if be.Details != nil {
			detail["details"] = be.Details
		}
		return textResult(fmt.Sprintf("%s: %s", be.Code, be.Message), detail, true)
	}
	return textResult("backend error: "+err.Error(), nil, true)
}

func (s *Server) callTool(r *http.Request, ont *spec.CompiledOntology, p auth.Principal, name string, args map[string]any,
	inputs map[string]json.RawMessage, state string, caps map[string]json.RawMessage) (map[string]any, *protoErr) {

	ctx, h := r.Context(), r.Header
	need := func(keys ...string) *protoErr {
		for _, k := range keys {
			if str(args, k) == "" {
				return &protoErr{Code: CodeInvalidParams, Message: fmt.Sprintf("argument %q is required", k)}
			}
		}
		return nil
	}
	typeOK := func() *protoErr {
		if ont.ObjectTypes[str(args, "objectType")] == nil {
			return &protoErr{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown objectType %q", str(args, "objectType"))}
		}
		return nil
	}
	switch name {
	case ToolDescribe:
		if t := str(args, "objectType"); t != "" && ont.ObjectTypes[t] == nil {
			return nil, &protoErr{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown objectType %q", t)}
		}
		structured, text := Describe(ont, str(args, "objectType"))
		return textResult(text, structured, false), nil
	case ToolGet:
		if e := need("objectType", "pk"); e != nil {
			return nil, e
		}
		if e := typeOK(); e != nil {
			return nil, e
		}
		return backendResult(s.Backend.GetObject(ctx, h, str(args, "objectType"), str(args, "pk"))), nil
	case ToolLinks:
		if e := need("objectType", "pk", "link"); e != nil {
			return nil, e
		}
		if e := typeOK(); e != nil {
			return nil, e
		}
		return backendResult(s.Backend.Links(ctx, h, str(args, "objectType"), str(args, "pk"), str(args, "link"), intArg(args, "pageSize"))), nil
	case ToolSearch, ToolAggregate:
		if e := need("objectType"); e != nil {
			return nil, e
		}
		if e := typeOK(); e != nil {
			return nil, e
		}
		body := map[string]any{}
		for k, v := range args {
			if k != "objectType" {
				body[k] = v
			}
		}
		if name == ToolSearch {
			if n := intArg(args, "pageSize"); n == 0 {
				body["pageSize"] = 20
			} else if n > 100 {
				body["pageSize"] = 100 // keep agent context small
			}
			return backendResult(s.Backend.Search(ctx, h, str(args, "objectType"), body)), nil
		}
		return backendResult(s.Backend.Aggregate(ctx, h, str(args, "objectType"), body)), nil
	case ToolValidate:
		at := ont.ActionTypes[str(args, "action")]
		if at == nil || !at.AI.ExposeToAgents {
			return nil, &protoErr{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown action %q", str(args, "action"))}
		}
		params, _ := args["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		return backendResult(s.Backend.Validate(ctx, h, at.APIName, params)), nil
	}
	if strings.HasPrefix(name, actionPrefix) {
		at := ont.ActionTypes[strings.TrimPrefix(name, actionPrefix)]
		if at != nil && at.AI.ExposeToAgents {
			return s.runAction(ctx, h, p, at, name, args, inputs, state, caps)
		}
	}
	return nil, &protoErr{Code: CodeInvalidParams, Message: "unknown tool: " + name}
}

const confirmKey = "confirm"

// runAction executes an exposed action. Actions with requireHumanConfirmation go through a Multi
// Round-Trip Request: the first call validates and returns an elicitation the client shows to the
// user; the retry carries the user's answer plus the signed requestState, which binds the
// confirmation to this caller, tool and exact arguments and doubles as the idempotency key.
func (s *Server) runAction(ctx context.Context, h http.Header, p auth.Principal, at *spec.ActionType, tool string,
	args map[string]any, inputs map[string]json.RawMessage, state string, caps map[string]json.RawMessage) (map[string]any, *protoErr) {

	if !at.AI.RequireHumanConfirmation {
		return backendResult(s.Backend.Apply(ctx, h, at.APIName, args, "")), nil
	}
	if _, ok := caps["elicitation"]; !ok && state == "" {
		return nil, &protoErr{Code: CodeMissingClientCapability,
			Message: "this action needs a human confirmation; the client must declare the elicitation capability",
			Data:    map[string]any{"requiredCapabilities": []string{"elicitation"}}}
	}
	if len(s.ConfirmSecret) < 16 {
		return textResult("this action requires human confirmation, but the server has no confirmation secret configured", nil, true), nil
	}
	if state == "" {
		// Validate first so the user is never asked to approve something that would fail anyway.
		if out, err := s.Backend.Validate(ctx, h, at.APIName, args); err != nil {
			return backendResult(out, err), nil
		}
		token, _ := newState(s.ConfirmSecret, tool, args, p.ID, s.now())
		return map[string]any{
			"resultType": "input_required",
			"inputRequests": map[string]any{confirmKey: map[string]any{
				"method": "elicitation/create",
				"params": map[string]any{
					"mode":    "form",
					"message": confirmMessage(at, args),
					"requestedSchema": map[string]any{"type": "object",
						"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "title": "Run this action"}},
						"required":   []string{"confirm"}},
				},
			}},
			"requestState": token,
		}, nil
	}
	st, err := verifyState(s.ConfirmSecret, state, tool, args, p.ID, s.now())
	if err != nil {
		return textResult(err.Error(), nil, true), nil
	}
	var answer struct {
		Action  string `json:"action"`
		Content struct {
			Confirm bool `json:"confirm"`
		} `json:"content"`
	}
	if raw, ok := inputs[confirmKey]; !ok || json.Unmarshal(raw, &answer) != nil {
		return nil, &protoErr{Code: CodeInvalidParams, Message: "inputResponses.confirm is required with requestState"}
	}
	if answer.Action != "accept" || !answer.Content.Confirm {
		return textResult("The user did not confirm the action; nothing was changed.", map[string]any{"status": "DECLINED"}, false), nil
	}
	return backendResult(s.Backend.Apply(ctx, h, at.APIName, args, "mcp-"+st.Nonce)), nil
}

func confirmMessage(at *spec.ActionType, args map[string]any) string {
	var b strings.Builder
	title := at.DisplayName
	if title == "" {
		title = at.APIName
	}
	fmt.Fprintf(&b, "An AI agent wants to run \"%s\".", title)
	if at.Description != "" {
		fmt.Fprintf(&b, " %s", at.Description)
	}
	b.WriteString("\nParameters:")
	for _, name := range at.ParamOrder {
		if v, ok := args[name]; ok {
			fmt.Fprintf(&b, "\n  %s = %v", name, v)
		}
	}
	return b.String()
}
