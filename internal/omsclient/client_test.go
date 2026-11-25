package omsclient

import (
	"context"
	"errors"
	"testing"

	"github.com/acme/ontology/internal/spec"
)

type fakeLoader struct {
	active   int64
	versions map[int64]*spec.CompiledOntology
	loads    int
}

func (f *fakeLoader) LoadActive(context.Context) (*spec.CompiledOntology, error) {
	f.loads++
	o := f.versions[f.active]
	if o == nil {
		return nil, errors.New("no active")
	}
	return o, nil
}

func (f *fakeLoader) LoadVersion(_ context.Context, v int64) (*spec.CompiledOntology, error) {
	f.loads++
	o := f.versions[v]
	if o == nil {
		return nil, errors.New("not found")
	}
	return o, nil
}

func ont(v int64) *spec.CompiledOntology {
	return &spec.CompiledOntology{APIName: "mro", Version: v, Checksum: "c" + string(rune('0'+v))}
}

func TestCacheLRUEviction(t *testing.T) {
	f := &fakeLoader{versions: map[int64]*spec.CompiledOntology{1: ont(1), 2: ont(2), 3: ont(3), 4: ont(4)}}
	c := New(f, 3)
	ctx := context.Background()
	for _, v := range []int64{1, 2, 3} {
		if _, err := c.Get(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	// touch v1 so v2 becomes the LRU victim
	if _, err := c.Get(ctx, 1); err != nil {
		t.Fatal(err)
	}
	loadsBefore := f.loads
	if _, err := c.Get(ctx, 4); err != nil { // evicts v2
		t.Fatal(err)
	}
	// v1 still cached (no new load), v2 evicted (reload needed)
	if _, err := c.Get(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if f.loads != loadsBefore+1 {
		t.Errorf("v1 should be a cache hit; loads=%d want %d", f.loads, loadsBefore+1)
	}
	c.Get(ctx, 2) //nolint:errcheck
	// Exactly two loads follow loadsBefore: Get(4) missed and Get(2) missed after eviction.
	// Get(1) in between was a hit, asserted above.
	if f.loads != loadsBefore+2 {
		t.Errorf("v2 should have been evicted and reloaded; loads=%d want %d", f.loads, loadsBefore+2)
	}
}

func TestActiveReloadsAfterVersionChange(t *testing.T) {
	f := &fakeLoader{active: 1, versions: map[int64]*spec.CompiledOntology{1: ont(1), 2: ont(2)}}
	c := New(f, 3)
	ctx := context.Background()
	o, _ := c.Active(ctx)
	if o.Version != 1 {
		t.Fatalf("active = %d", o.Version)
	}
	f.active = 2
	c.OnVersionChange(2)
	select {
	case v := <-c.Subscribe():
		if v != 2 {
			t.Errorf("notify = %d", v)
		}
	default:
		t.Error("expected a notification")
	}
	o, _ = c.Active(ctx)
	if o.Version != 2 {
		t.Errorf("active after change = %d, want 2", o.Version)
	}
}
