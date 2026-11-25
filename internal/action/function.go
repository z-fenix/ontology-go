package action

import (
	"context"
	"fmt"
	"sync"

	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

// MaxFunctionReads bounds the objects one function invocation may read (Spec §7.7).
const MaxFunctionReads = 100000

// Reader is the read-only, permission-scoped view of the ontology a function receives.
type Reader interface {
	Get(ctx context.Context, ref objstore.ObjectRef) (*objstore.Object, error)
	Find(ctx context.Context, typ string, f objstore.Filter, limit int) ([]*objstore.Object, error)
}

// FuncInput is passed to a Go function implementation.
type FuncInput struct {
	Params  map[string]any                // primitive parameters
	Objects map[string]*objstore.Object   // objectRef parameters, loaded
	Lists   map[string][]*objstore.Object // objectRefList parameters, loaded
	Reader  Reader
	Actor   Actor
}

// Func returns the edits an action should make. It must not write anything itself.
type Func func(ctx context.Context, in FuncInput) ([]objstore.Edit, error)

// FunctionRegistry maps a function ref (actionType.function.ref) to a Go implementation.
// TypeScript/Python runtimes are a later iteration; their refs are simply not registered.
type FunctionRegistry struct {
	mu    sync.RWMutex
	funcs map[string]Func
}

// NewFunctionRegistry builds an empty registry.
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{funcs: map[string]Func{}}
}

// Register adds an implementation for ref.
func (r *FunctionRegistry) Register(ref string, f Func) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[ref] = f
}

// Lookup returns the implementation for ref.
func (r *FunctionRegistry) Lookup(ref string) (Func, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.funcs[ref]
	return f, ok
}

type scopedReader struct {
	ont    *spec.CompiledOntology
	loader ObjectLoader
	e      *Engine
	actor  Actor
	mu     sync.Mutex
	reads  int
}

func (r *scopedReader) count(n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads += n
	if r.reads > MaxFunctionReads {
		return fmt.Errorf("function exceeded the read limit of %d objects", MaxFunctionReads)
	}
	return nil
}

func (r *scopedReader) visible(ctx context.Context, o *objstore.Object) (bool, error) {
	ot := r.ont.ObjectTypes[o.Ref.Type]
	if ot == nil || ot.Authz == nil {
		return true, nil
	}
	return r.e.allowed(ctx, r.actor, "viewer", ot.Authz.FGAType+":"+o.Ref.PK)
}

func (r *scopedReader) Get(ctx context.Context, ref objstore.ObjectRef) (*objstore.Object, error) {
	if err := r.count(1); err != nil {
		return nil, err
	}
	found, err := r.loader.GetMany(ctx, r.ont, []objstore.ObjectRef{ref})
	if err != nil {
		return nil, err
	}
	o, ok := found[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", objstore.ErrNotFound, ref)
	}
	if ok, err := r.visible(ctx, o); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s", objstore.ErrNotFound, ref) // invisible == absent
	}
	return o, nil
}

func (r *scopedReader) Find(ctx context.Context, typ string, f objstore.Filter, limit int) ([]*objstore.Object, error) {
	objs, _, err := r.loader.Find(ctx, r.ont, typ, f, objstore.Page{Size: limit})
	if err != nil {
		return nil, err
	}
	if err := r.count(len(objs)); err != nil {
		return nil, err
	}
	out := objs[:0]
	for _, o := range objs {
		ok, err := r.visible(ctx, o)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, o)
		}
	}
	return out, nil
}
