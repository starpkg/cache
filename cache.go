// Package cache provides a Starlark module offering bounded, in-memory
// key-value caches with TTL expiry.
//
// Each cache from new_cache() is an independent namespace. Stored values are
// snapshotted through the serial module (a lossless dumps/loads round-trip), so
// a cached value is an immutable, independent copy: mutating what you put in or
// what you get out never affects the stored entry. Caches are bounded — once
// max_entries is reached, the oldest entry is evicted. Only serializable values
// can be cached (functions and the like are rejected by serial).
package cache

import (
	"fmt"
	"sync"
	"time"

	"github.com/1set/starlet"
	"github.com/1set/starlet/lib/serial"
	"github.com/starpkg/base"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// ModuleName is the name used in Starlark's load() for this module.
const ModuleName = "cache"

const (
	configKeyMaxEntries = "max_entries"
	configKeyDefaultTTL = "default_ttl"
)

const defaultMaxEntries = 128

var none = starlark.None

// Module wraps a ConfigurableModule with cache-specific functions.
type Module struct {
	cfgMod *base.ConfigurableModule
	ext    *base.ConfigurableModuleExt
	clock  func() time.Time
	dumps  *starlark.Builtin
	loads  *starlark.Builtin
}

// NewModule creates a Module using the wall clock.
func NewModule() *Module { return newModule(time.Now) }

// NewModuleWithClock creates a Module with an injected clock (for deterministic
// TTL testing). clock must be non-nil.
func NewModuleWithClock(clock func() time.Time) *Module { return newModule(clock) }

func newModule(clock func() time.Time) *Module {
	cm, _ := base.NewConfigurableModuleWithConfigOptions(
		genConfigOption(configKeyMaxEntries, "Default maximum entries per cache", defaultMaxEntries),
		genConfigOption(configKeyDefaultTTL, "Default time-to-live in seconds (0 = no expiry)", 0),
	)
	sd, _ := serial.LoadModule()
	mod := sd[serial.ModuleName].(*starlarkstruct.Module)
	return &Module{
		cfgMod: cm,
		ext:    cm.Extend(),
		clock:  clock,
		dumps:  mod.Members["dumps"].(*starlark.Builtin),
		loads:  mod.Members["loads"].(*starlark.Builtin),
	}
}

func genConfigOption[T any](name, description string, defaultValue T) *base.ConfigOption[T] {
	return base.NewConfigOption(defaultValue).
		WithName(name).
		WithDescription(description).
		WithEnvVar("CACHE_" + upper(name))
}

// LoadModule returns the Starlark module loader.
func (m *Module) LoadModule() starlet.ModuleLoader {
	funcs := starlark.StringDict{
		"new_cache": starlark.NewBuiltin(ModuleName+".new_cache", m.newCache),
	}
	return m.cfgMod.LoadModule(ModuleName, funcs)
}

// newCache builds a cache instance.
//
//	new_cache(max_entries=<config>, ttl=<config>) -> Cache
func (m *Module) newCache(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	maxEntries := m.ext.GetInt(configKeyMaxEntries)
	ttlSec := m.ext.GetInt(configKeyDefaultTTL)
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"max_entries?", &maxEntries,
		"ttl?", &ttlSec,
	); err != nil {
		return none, err
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	if ttlSec < 0 {
		return none, fmt.Errorf("%s: ttl must not be negative", b.Name())
	}
	return &cacheValue{
		entries:    make(map[string]cacheEntry),
		maxEntries: maxEntries,
		defaultTTL: time.Duration(ttlSec) * time.Second,
		clock:      m.clock,
		dumps:      m.dumps,
		loads:      m.loads,
	}, nil
}

func upper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// cacheEntry is one stored value: its serial-encoded form and optional expiry.
type cacheEntry struct {
	data      string
	expiresAt time.Time // zero == no expiry
}

// cacheValue is a bounded, TTL-aware key-value store exposed to Starlark.
type cacheValue struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	order      []string // insertion order, for FIFO eviction
	maxEntries int
	defaultTTL time.Duration
	clock      func() time.Time
	dumps      *starlark.Builtin
	loads      *starlark.Builtin
}

var (
	_ starlark.Value    = (*cacheValue)(nil)
	_ starlark.HasAttrs = (*cacheValue)(nil)
)

func (c *cacheValue) String() string        { return fmt.Sprintf("<cache.Cache max=%d>", c.maxEntries) }
func (c *cacheValue) Type() string          { return "cache.Cache" }
func (c *cacheValue) Freeze()               {}
func (c *cacheValue) Truth() starlark.Bool  { return starlark.True }
func (c *cacheValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: cache.Cache") }

func (c *cacheValue) AttrNames() []string {
	return []string{"set", "get", "has", "delete", "clear", "keys", "size"}
}

func (c *cacheValue) Attr(name string) (starlark.Value, error) {
	fn, ok := map[string]func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error){
		"set":    c.set,
		"get":    c.get,
		"has":    c.has,
		"delete": c.delete,
		"clear":  c.clear,
		"keys":   c.keys,
		"size":   c.size,
	}[name]
	if !ok {
		return nil, nil
	}
	return starlark.NewBuiltin("cache.Cache."+name, fn), nil
}

// expired reports whether e is expired as of now. Caller holds the lock.
func (c *cacheValue) expired(e cacheEntry, now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

func (c *cacheValue) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// set stores value under key.
//
//	Cache.set(key, value, ttl=None) -> None
func (c *cacheValue) set(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		key   string
		value starlark.Value
		ttl   = starlark.MakeInt(-1) // -1 => use default
	)
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "key", &key, "value", &value, "ttl?", &ttl); err != nil {
		return none, err
	}
	ttlSec, ok := ttl.Int64()
	if !ok {
		return none, fmt.Errorf("%s: ttl out of range", b.Name())
	}

	// Snapshot the value losslessly via serial (independent, immutable copy).
	dumped, err := c.dumps.CallInternal(thread, starlark.Tuple{value}, nil)
	if err != nil {
		return none, fmt.Errorf("cache: value is not serializable: %w", err)
	}
	data, ok := dumped.(starlark.String)
	if !ok {
		return none, fmt.Errorf("cache: serial.dumps did not return a string")
	}

	var expiresAt time.Time
	d := c.defaultTTL
	if ttlSec >= 0 {
		d = time.Duration(ttlSec) * time.Second
	}
	if d > 0 {
		expiresAt = c.clock().Add(d)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = cacheEntry{data: string(data), expiresAt: expiresAt}
	// Evict oldest entries while over the bound.
	for len(c.order) > c.maxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	return none, nil
}

// get returns the value for key, or default (None) if missing or expired.
//
//	Cache.get(key, default=None) -> value
func (c *cacheValue) get(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		key string
		def starlark.Value = none
	)
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "key", &key, "default?", &def); err != nil {
		return none, err
	}

	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && c.expired(e, c.clock()) {
		delete(c.entries, key)
		c.removeOrder(key)
		ok = false
	}
	c.mu.Unlock()

	if !ok {
		return def, nil
	}
	// Decode a fresh, independent value.
	v, err := c.loads.CallInternal(thread, starlark.Tuple{starlark.String(e.data)}, nil)
	if err != nil {
		return none, fmt.Errorf("cache: failed to decode stored value: %w", err)
	}
	return v, nil
}

// has reports whether key is present and not expired.
func (c *cacheValue) has(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "key", &key); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && c.expired(e, c.clock()) {
		delete(c.entries, key)
		c.removeOrder(key)
		ok = false
	}
	return starlark.Bool(ok), nil
}

// delete removes key, returning whether it was present.
func (c *cacheValue) delete(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "key", &key); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	if ok {
		delete(c.entries, key)
		c.removeOrder(key)
	}
	return starlark.Bool(ok), nil
}

// clear removes all entries.
func (c *cacheValue) clear(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
	c.order = nil
	return none, nil
}

// keys returns the non-expired keys in insertion order.
func (c *cacheValue) keys(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	var out []starlark.Value
	kept := c.order[:0:0]
	for _, k := range c.order {
		e := c.entries[k]
		if c.expired(e, now) {
			delete(c.entries, k)
			continue
		}
		kept = append(kept, k)
		out = append(out, starlark.String(k))
	}
	c.order = kept
	return starlark.NewList(out), nil
}

// size returns the number of non-expired entries.
func (c *cacheValue) size(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	n := 0
	for _, e := range c.entries {
		if !c.expired(e, now) {
			n++
		}
	}
	return starlark.MakeInt(n), nil
}
