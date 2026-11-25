// Package omsclient is the read-side client with a version cache (Spec §5.6).
// It is used by the Action Service to resolve compiled ontologies.
package omsclient

import (
	"container/list"
	"context"
	"sync"

	"github.com/acme/ontology/internal/spec"
)

// Provider resolves compiled ontologies by version, and the active version.
type Provider interface {
	Active(ctx context.Context) (*spec.CompiledOntology, error)
	Get(ctx context.Context, version int64) (*spec.CompiledOntology, error)
	Subscribe() <-chan int64
}

// Loader fetches compiled ontologies from a source (HTTP OMS, or the store directly in tests).
type Loader interface {
	LoadActive(ctx context.Context) (*spec.CompiledOntology, error)
	LoadVersion(ctx context.Context, version int64) (*spec.CompiledOntology, error)
}

// Cache is an LRU of compiled ontology versions with an active pointer (Spec §5.6).
type Cache struct {
	loader   Loader
	maxItems int

	mu       sync.RWMutex
	byVer    map[int64]*list.Element
	lru      *list.List
	active   int64
	notify   chan int64
}

type entry struct {
	version  int64
	compiled *spec.CompiledOntology
}

// New builds a cache keeping up to maxItems versions (default 3).
func New(loader Loader, maxItems int) *Cache {
	if maxItems <= 0 {
		maxItems = 3
	}
	return &Cache{
		loader:   loader,
		maxItems: maxItems,
		byVer:    map[int64]*list.Element{},
		lru:      list.New(),
		notify:   make(chan int64, 8),
	}
}

// Active returns the current active ontology, loading and caching it.
func (c *Cache) Active(ctx context.Context) (*spec.CompiledOntology, error) {
	o, err := c.loader.LoadActive(ctx)
	if err != nil {
		return nil, err
	}
	c.put(o)
	c.mu.Lock()
	c.active = o.Version
	c.mu.Unlock()
	return o, nil
}

// Get returns a specific version from cache, loading it on a miss.
func (c *Cache) Get(ctx context.Context, version int64) (*spec.CompiledOntology, error) {
	c.mu.Lock()
	if el, ok := c.byVer[version]; ok {
		c.lru.MoveToFront(el)
		o := el.Value.(*entry).compiled
		c.mu.Unlock()
		return o, nil
	}
	c.mu.Unlock()

	o, err := c.loader.LoadVersion(ctx, version)
	if err != nil {
		return nil, err
	}
	c.put(o)
	return o, nil
}

// OnVersionChange is called by the LISTEN/NOTIFY loop; it evicts so the next Active reloads.
func (c *Cache) OnVersionChange(version int64) {
	c.mu.Lock()
	c.active = 0 // force reload on next Active()
	c.mu.Unlock()
	select {
	case c.notify <- version:
	default:
	}
}

// Subscribe returns a channel of newly-activated versions.
func (c *Cache) Subscribe() <-chan int64 { return c.notify }

func (c *Cache) put(o *spec.CompiledOntology) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byVer[o.Version]; ok {
		el.Value = &entry{o.Version, o}
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&entry{o.Version, o})
	c.byVer[o.Version] = el
	for c.lru.Len() > c.maxItems {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.lru.Remove(back)
		delete(c.byVer, back.Value.(*entry).version)
	}
}
