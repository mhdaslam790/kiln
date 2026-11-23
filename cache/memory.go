package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Memory is an in-process LRU Store bounded by entry count and total bytes.
// Each application instance has its own Memory store, so writes made by one
// instance do not invalidate another instance's entries until they expire;
// use a shared store (e.g. Redis) when running several instances.
type Memory struct {
	mu         sync.Mutex
	ll         *list.List
	items      map[string]*list.Element
	bytes      int64
	maxEntries int
	maxBytes   int64
	now        func() time.Time
}

type memEntry struct {
	key     string
	value   []byte
	expires time.Time // zero = never
}

// MemoryOption configures a Memory store.
type MemoryOption func(*Memory)

// MaxEntries caps the number of entries (default 10,000; <= 0 = unlimited).
func MaxEntries(n int) MemoryOption { return func(m *Memory) { m.maxEntries = n } }

// MaxBytes caps the total size of keys and values (default 64 MiB;
// <= 0 = unlimited).
func MaxBytes(n int64) MemoryOption { return func(m *Memory) { m.maxBytes = n } }

// WithClock overrides time.Now (for tests).
func WithClock(now func() time.Time) MemoryOption { return func(m *Memory) { m.now = now } }

// NewMemory returns an empty in-memory store.
func NewMemory(opts ...MemoryOption) *Memory {
	m := &Memory{
		ll:         list.New(),
		items:      map[string]*list.Element{},
		maxEntries: 10_000,
		maxBytes:   64 << 20,
		now:        time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Get implements Store. It returns a copy of the stored value.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*memEntry)
	if !e.expires.IsZero() && !m.now().Before(e.expires) {
		m.remove(el)
		return nil, false, nil
	}
	m.ll.MoveToFront(el)
	return append([]byte(nil), e.value...), true, nil
}

// Set implements Store.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	e := &memEntry{key: key, value: append([]byte(nil), value...)}
	if ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	size := entrySize(e)
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	if m.maxBytes > 0 && size > m.maxBytes {
		return nil // too large to ever fit; skip silently
	}
	m.items[key] = m.ll.PushFront(e)
	m.bytes += size
	for (m.maxEntries > 0 && m.ll.Len() > m.maxEntries) || (m.maxBytes > 0 && m.bytes > m.maxBytes) {
		m.remove(m.ll.Back())
	}
	return nil
}

// Delete implements Store.
func (m *Memory) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		if el, ok := m.items[k]; ok {
			m.remove(el)
		}
	}
	return nil
}

// Len returns the number of entries, including expired ones not yet evicted.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ll.Len()
}

// Clear removes every entry.
func (m *Memory) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ll.Init()
	m.items = map[string]*list.Element{}
	m.bytes = 0
}

func (m *Memory) remove(el *list.Element) {
	e := m.ll.Remove(el).(*memEntry)
	delete(m.items, e.key)
	m.bytes -= entrySize(e)
}

func entrySize(e *memEntry) int64 { return int64(len(e.key) + len(e.value) + 48) }
