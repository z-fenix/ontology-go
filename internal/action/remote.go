package action

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/platform/apperr"
	"github.com/acme/ontology/internal/platform/httpx"
	"github.com/acme/ontology/internal/spec"
)

// Remote function runtimes (TypeScript, Python) run as separate services. For each invocation the
// engine POSTs {ref, actor, params, objects, lists, reader} to <URL>/functions/<ref> and gets back
// {"edits": [...]}. The function reads further objects through reader.url, a callback into this
// service that is valid only while the invocation runs and goes through the same viewer checks and
// read limit as Go functions. Returned edits are validated and committed like any other edits;
// the runtime itself never writes. See runtimes/README.md for the wire format.

// MaxFunctionResponse bounds a runtime's response body.
const MaxFunctionResponse = 4 << 20

// RemoteRuntime calls functions hosted by an out-of-process runtime.
type RemoteRuntime struct {
	// Name is the runtime as written in action type YAML (typescript | python).
	Name string
	// URL is the runtime service's base URL, e.g. http://py-functions:9100.
	URL string
	// CallbackURL is this service's base URL as reachable from the runtime.
	CallbackURL string
	// Token, when set, is sent as a bearer token to the runtime.
	Token string
	// Timeout bounds one invocation (default 10s).
	Timeout time.Duration
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Callbacks holds the reader endpoints of running invocations.
	Callbacks *FunctionCallbacks
}

type wireRef struct {
	Type string `json:"type"`
	PK   string `json:"pk"`
}

type wireObject struct {
	Type    string         `json:"type"`
	PK      string         `json:"pk"`
	Version int64          `json:"version"`
	Props   map[string]any `json:"props"`
}

type wireActor struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
}

type wireReader struct {
	URL string `json:"url"`
}

type functionRequest struct {
	Ref     string                   `json:"ref"`
	Actor   wireActor                `json:"actor"`
	Params  map[string]any           `json:"params"`
	Objects map[string]*wireObject   `json:"objects"`
	Lists   map[string][]*wireObject `json:"lists"`
	Reader  wireReader               `json:"reader"`
}

type wireEdit struct {
	Op              string         `json:"op"`
	Type            string         `json:"type"`
	PK              string         `json:"pk"`
	Changes         map[string]any `json:"changes,omitempty"`
	ExpectedVersion int64          `json:"expectedVersion,omitempty"`
	LinkType        string         `json:"linkType,omitempty"`
	Target          *wireRef       `json:"target,omitempty"`
}

type functionResponse struct {
	Edits []wireEdit `json:"edits"`
}

type functionError struct {
	Code    string `json:"errorCode"`
	Message string `json:"message"`
}

type findRequest struct {
	Type  string           `json:"type"`
	Eq    map[string]any   `json:"eq,omitempty"`
	In    map[string][]any `json:"in,omitempty"`
	Limit int              `json:"limit,omitempty"`
}

// FunctionCallbacks serves the read callbacks of running remote invocations.
type FunctionCallbacks struct {
	mu   sync.Mutex
	live map[string]*invocation
}

type invocation struct {
	ctx    context.Context
	ont    *spec.CompiledOntology
	reader Reader
}

// NewFunctionCallbacks builds an empty callback registry.
func NewFunctionCallbacks() *FunctionCallbacks {
	return &FunctionCallbacks{live: map[string]*invocation{}}
}

func (c *FunctionCallbacks) add(inv *invocation) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live[token] = inv
	return token, nil
}

func (c *FunctionCallbacks) remove(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, token)
}

func (c *FunctionCallbacks) get(token string) *invocation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[token]
}

// Handler serves POST /{token}/get and POST /{token}/find; mount it at /internal/function-reads.
func (c *FunctionCallbacks) Handler() http.Handler {
	r := chi.NewRouter()
	r.Post("/{token}/get", c.readGet)
	r.Post("/{token}/find", c.readFind)
	return r
}

func toWire(o *objstore.Object) *wireObject {
	if o == nil {
		return nil
	}
	return &wireObject{Type: o.Ref.Type, PK: o.Ref.PK, Version: o.Version, Props: o.Props}
}

func (c *FunctionCallbacks) lookup(w http.ResponseWriter, r *http.Request) *invocation {
	inv := c.get(chi.URLParam(r, "token"))
	if inv == nil {
		httpx.WriteError(w, r, apperr.New(http.StatusNotFound, "LOCATION_NOT_FOUND", "no running function invocation"))
	}
	return inv
}

func readError(err error) error {
	if errors.Is(err, objstore.ErrNotFound) {
		return apperr.New(http.StatusNotFound, "OBJECT_NOT_FOUND", "%v", err)
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae
	}
	return apperr.New(http.StatusUnprocessableEntity, "FUNCTION_READ_FAILED", "%v", err)
}

func (c *FunctionCallbacks) readGet(w http.ResponseWriter, r *http.Request) {
	inv := c.lookup(w, r)
	if inv == nil {
		return
	}
	var ref wireRef
	if err := httpx.DecodeJSON(r, &ref); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	o, err := inv.reader.Get(inv.ctx, objstore.ObjectRef{Type: ref.Type, PK: ref.PK})
	if err != nil {
		httpx.WriteError(w, r, readError(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toWire(o))
}

func (c *FunctionCallbacks) readFind(w http.ResponseWriter, r *http.Request) {
	inv := c.lookup(w, r)
	if inv == nil {
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var req findRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, r, apperr.BadRequest("INVALID_BODY", "invalid request body: %v", err))
		return
	}
	ot := inv.ont.ObjectTypes[req.Type]
	if ot == nil {
		httpx.WriteError(w, r, apperr.BadRequest("UNKNOWN_OBJECT_TYPE", "unknown object type %q", req.Type))
		return
	}
	f := objstore.Filter{Eq: map[string]any{}, In: map[string][]any{}}
	for k, v := range req.Eq {
		cv, err := filterValue(ot, k, v)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		f.Eq[k] = cv
	}
	for k, vs := range req.In {
		for _, v := range vs {
			cv, err := filterValue(ot, k, v)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			f.In[k] = append(f.In[k], cv)
		}
	}
	limit := req.Limit
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	objs, err := inv.reader.Find(inv.ctx, req.Type, f, limit)
	if err != nil {
		httpx.WriteError(w, r, readError(err))
		return
	}
	out := make([]*wireObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, toWire(o))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func filterValue(ot *spec.ObjectType, prop string, v any) (any, error) {
	p := ot.Properties[prop]
	if p == nil {
		return nil, apperr.BadRequest("UNKNOWN_PROPERTY", "%s has no property %q", ot.APIName, prop)
	}
	if !scalarBase(p.Type.Base) {
		return nil, apperr.BadRequest("FIELD_NOT_FILTERABLE", "%s.%s cannot be filtered on", ot.APIName, prop)
	}
	cv, err := coerce(p.Type.Base, v)
	if err != nil {
		return nil, apperr.BadRequest("INVALID_FILTER", "%s.%s: %v", ot.APIName, prop, err)
	}
	return cv, nil
}

func scalarBase(b spec.BaseType) bool {
	switch b {
	case spec.TString, spec.TAttachment, spec.TInteger, spec.TLong, spec.TDouble, spec.TDecimal,
		spec.TBoolean, spec.TTimestamp, spec.TDate:
		return true
	}
	return false
}

// Func returns a Func that invokes ref in this runtime.
func (rt *RemoteRuntime) Func(ont *spec.CompiledOntology, ref string) Func {
	return func(ctx context.Context, in FuncInput) ([]objstore.Edit, error) {
		if rt.Callbacks == nil || rt.CallbackURL == "" {
			return nil, apperr.New(http.StatusNotImplemented, "FUNCTION_RUNTIME_UNAVAILABLE",
				"runtime %s has no callback URL configured", rt.Name)
		}
		timeout := rt.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		token, err := rt.Callbacks.add(&invocation{ctx: cctx, ont: ont, reader: in.Reader})
		if err != nil {
			return nil, err
		}
		defer rt.Callbacks.remove(token)

		req := functionRequest{
			Ref:     ref,
			Actor:   wireActor{ID: in.Actor.ID, Kind: string(in.Actor.Kind), OnBehalfOf: in.Actor.OnBehalfOf},
			Params:  in.Params,
			Objects: map[string]*wireObject{},
			Lists:   map[string][]*wireObject{},
			Reader:  wireReader{URL: strings.TrimRight(rt.CallbackURL, "/") + "/internal/function-reads/" + token},
		}
		for name, o := range in.Objects {
			req.Objects[name] = toWire(o)
		}
		for name, list := range in.Lists {
			for _, o := range list {
				req.Lists[name] = append(req.Lists[name], toWire(o))
			}
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		hr, err := http.NewRequestWithContext(cctx, http.MethodPost,
			strings.TrimRight(rt.URL, "/")+"/functions/"+url.PathEscape(ref), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Content-Type", "application/json")
		if rt.Token != "" {
			hr.Header.Set("Authorization", "Bearer "+rt.Token)
		}
		client := rt.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(hr)
		if err != nil {
			return nil, apperr.New(http.StatusBadGateway, "FUNCTION_RUNTIME_UNAVAILABLE",
				"cannot reach the %s function runtime", rt.Name).Wrap(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxFunctionResponse+1))
		if err != nil {
			return nil, apperr.New(http.StatusBadGateway, "FUNCTION_FAILED", "reading the %s runtime response", rt.Name).Wrap(err)
		}
		if len(raw) > MaxFunctionResponse {
			return nil, apperr.New(http.StatusBadGateway, "FUNCTION_FAILED", "function %s returned more than %d bytes", ref, MaxFunctionResponse)
		}
		if resp.StatusCode != http.StatusOK {
			var fe functionError
			_ = json.Unmarshal(raw, &fe)
			if resp.StatusCode == http.StatusUnprocessableEntity {
				if fe.Code == "" {
					fe.Code = "FUNCTION_REJECTED"
				}
				return nil, apperr.New(http.StatusUnprocessableEntity, fe.Code, "%s", fe.Message)
			}
			return nil, apperr.New(http.StatusBadGateway, "FUNCTION_FAILED",
				"function %s failed in the %s runtime (HTTP %d): %s", ref, rt.Name, resp.StatusCode, fe.Message)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var out functionResponse
		if err := dec.Decode(&out); err != nil {
			return nil, apperr.New(http.StatusBadGateway, "FUNCTION_FAILED", "function %s returned an invalid response: %v", ref, err)
		}
		return decodeEdits(ont, out.Edits)
	}
}

// decodeEdits converts wire edits, coercing changed values to their property types.
func decodeEdits(ont *spec.CompiledOntology, in []wireEdit) ([]objstore.Edit, error) {
	bad := func(i int, format string, args ...any) error {
		return apperr.New(http.StatusUnprocessableEntity, "EDIT_VALIDATION_FAILED", "function edit %d: %s", i, fmt.Sprintf(format, args...))
	}
	out := make([]objstore.Edit, 0, len(in))
	for i, we := range in {
		op := objstore.EditOp(strings.ToUpper(we.Op))
		switch op {
		case objstore.OpCreate, objstore.OpModify, objstore.OpDelete, objstore.OpLink, objstore.OpUnlink:
		default:
			return nil, bad(i, "unknown op %q", we.Op)
		}
		if we.Type == "" || we.PK == "" {
			return nil, bad(i, "type and pk are required")
		}
		ed := objstore.Edit{Op: op, Ref: objstore.ObjectRef{Type: we.Type, PK: we.PK},
			ExpectedVersion: we.ExpectedVersion, LinkType: we.LinkType}
		if op == objstore.OpLink || op == objstore.OpUnlink {
			if we.LinkType == "" || we.Target == nil || we.Target.Type == "" || we.Target.PK == "" {
				return nil, bad(i, "%s needs linkType and target", op)
			}
			ed.Target = objstore.ObjectRef{Type: we.Target.Type, PK: we.Target.PK}
		}
		if len(we.Changes) > 0 {
			ot := ont.ObjectTypes[we.Type]
			ed.Changes = make(map[string]any, len(we.Changes))
			for k, v := range we.Changes {
				ed.Changes[k] = v
				if ot == nil || v == nil {
					continue // unknown types and properties are reported by edit validation
				}
				p := ot.Properties[k]
				if p == nil || !scalarBase(p.Type.Base) {
					continue
				}
				cv, err := coerce(p.Type.Base, v)
				if err != nil {
					return nil, bad(i, "%s.%s: %v", we.Type, k, err)
				}
				ed.Changes[k] = cv
			}
		}
		out = append(out, ed)
	}
	return out, nil
}

// ParseRuntimes parses "python=http://py:9100,typescript=http://ts:9200".
func ParseRuntimes(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, u, ok := strings.Cut(part, "=")
		name, u = strings.TrimSpace(name), strings.TrimSpace(u)
		if !ok || (name != "python" && name != "typescript") {
			return nil, fmt.Errorf("invalid function runtime %q: want python=<url> or typescript=<url>", part)
		}
		if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return nil, fmt.Errorf("invalid URL for runtime %s: %q", name, u)
		}
		out[name] = u
	}
	return out, nil
}
