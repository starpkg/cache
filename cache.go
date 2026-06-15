// Package cache provides a Starlark module offering bounded, in-memory
// key-value caches with TTL expiry.
//
// Design summary (the README's "Design & semantics" section is the long form):
//
//   - Namespacing: each new_cache() is an INDEPENDENT instance. There is no
//     central Manager/registry — to separate namespaces, create separate caches.
//   - Eviction is FIFO (oldest-inserted evicted first), NOT LRU. FIFO needs no
//     per-access bookkeeping, so it is simple and predictable; the tradeoff is
//     that a frequently read ("hot") key is not protected from eviction.
//   - The bound is max_entries (default 128, host-configurable). Caches never
//     hold more live entries than that.
//   - Values are stored as an immutable serial snapshot (serial.dumps -> string
//     on set, serial.loads -> fresh value on get), so a cached value is a
//     lossless, independent copy: mutating what you put in, or what you get out,
//     never affects the stored entry. Only serializable values can be cached;
//     a non-serializable value makes set() return an error — there is no silent
//     variant, and no path panics the host.
//   - Concurrency: every operation is guarded by a sync.Mutex, so reads and
//     writes are serializable and each call sees a consistent snapshot.
//   - TTL expiry is lazy: an entry is purged on per-key access (get/has/delete)
//     and on keys()/size() scans, never by a background goroutine. The clock is
//     injectable via NewModuleWithClock for deterministic TTL testing.
package cache

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/1set/starlet"
	"github.com/1set/starlet/dataconv/types"
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
		WithEnvVar(strings.ToUpper(ModuleName + "_" + name))
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

// cacheEntry is one stored value: its serial-encoded form and optional expiry.
//
// data is an immutable serial snapshot of the cached Starlark value
// (serial.dumps -> string at set time). On get it is decoded back with
// serial.loads into a fresh, independent value, so neither the value the caller
// stored nor any value the caller later reads can mutate the stored entry.
type cacheEntry struct {
	data      string
	expiresAt time.Time // zero == no expiry
}

// cacheValue is a bounded, TTL-aware key-value store exposed to Starlark.
//
// Design (see package doc and README for the full rationale):
//
//   - Eviction is FIFO — the oldest *inserted* entry is evicted first when the
//     store grows past maxEntries. This is deliberately NOT LRU: FIFO needs no
//     per-access bookkeeping (a get never touches order), making it simple and
//     predictable at the cost of not favouring hot keys. order holds the keys in
//     insertion order; eviction pops from the front (order[0]).
//   - The bound is maxEntries; the store never holds more live entries than that.
//   - Values are stored as an immutable serial snapshot (string) and decoded into
//     a fresh value on get, giving independent copies (see cacheEntry).
//   - All operations take mu, so reads and writes are serializable: every method
//     observes a consistent snapshot of entries/order under the lock.
//   - TTL expiry is lazy: nothing runs in the background. An entry is purged when
//     it is next touched (get/has/delete per key) or scanned (keys/size). clock
//     is injectable (NewModuleWithClock) for deterministic TTL tests.
type cacheValue struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	order      []string // insertion order, for FIFO eviction (oldest first)
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
	// Each method is registered under a static literal name so the static
	// doc-coverage scanner (1set/meta/doccov) can see the script-facing
	// surface; the name string is otherwise only used in error messages.
	switch name {
	case "set":
		return starlark.NewBuiltin("set", c.set), nil
	case "get":
		return starlark.NewBuiltin("get", c.get), nil
	case "has":
		return starlark.NewBuiltin("has", c.has), nil
	case "delete":
		return starlark.NewBuiltin("delete", c.delete), nil
	case "clear":
		return starlark.NewBuiltin("clear", c.clear), nil
	case "keys":
		return starlark.NewBuiltin("keys", c.keys), nil
	case "size":
		return starlark.NewBuiltin("size", c.size), nil
	}
	return nil, nil
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
//
// ttl: None or absent => use the cache default; a non-negative int => that
// per-entry TTL in seconds (0 = no expiry); a negative int is an error
// (symmetry with new_cache, which rejects a negative default ttl).
//
// value is snapshotted via serial.dumps into an immutable string before it is
// stored, so the stored entry is independent of the caller's value. There is no
// silent "best effort" variant: a value serial cannot encode (a function, a
// builtin, …) makes set return a clean error rather than panicking or storing
// nothing — the error names "serializable" so callers can detect it.
//
// After writing under the FIFO bound, the oldest inserted keys (order[0]…) are
// evicted until len(order) <= maxEntries.
func (c *cacheValue) set(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		key   string
		value starlark.Value
		ttl   = types.NewNullableInt(starlark.MakeInt(-1)) // None/absent => use default
	)
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "key", &key, "value", &value, "ttl?", ttl); err != nil {
		return none, err
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
	if !ttl.IsNull() {
		ttlSec, ok := ttl.Value().Int64()
		if !ok {
			return none, fmt.Errorf("%s: ttl out of range", b.Name())
		}
		if ttlSec < 0 {
			return none, fmt.Errorf("%s: ttl must not be negative", b.Name())
		}
		d = time.Duration(ttlSec) * time.Second
	}
	if d > 0 {
		expiresAt = c.clock().Add(d)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// A re-set of an existing key keeps its original insertion position (it is
	// not bumped to the back), so FIFO eviction is by first-insertion age.
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = cacheEntry{data: string(data), expiresAt: expiresAt}
	// FIFO eviction: while over the bound, drop the oldest-inserted key.
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
//
// A live entry is decoded with serial.loads into a fresh, independent value, so
// mutating the returned value never touches the stored snapshot. If a stored
// snapshot fails to decode, get returns a clean error rather than panicking.
// Looking up a key is read-only with respect to FIFO order (get never reorders);
// the only mutation get performs is lazily purging the key if it has expired.
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

// purgeExpired drops every expired entry from both the map and the order slice,
// returning the surviving keys in insertion order. Caller holds the lock.
func (c *cacheValue) purgeExpired(now time.Time) []string {
	kept := c.order[:0:0]
	for _, k := range c.order {
		e := c.entries[k]
		if c.expired(e, now) {
			delete(c.entries, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
	return kept
}

// keys returns the non-expired keys in insertion order.
func (c *cacheValue) keys(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.purgeExpired(c.clock())
	out := make([]starlark.Value, len(kept))
	for i, k := range kept {
		out[i] = starlark.String(k)
	}
	return starlark.NewList(out), nil
}

// size returns the number of non-expired entries. Like keys(), it lazily purges
// expired entries while scanning (under the lock) so dead entries never linger
// counting toward the FIFO max_entries bound.
func (c *cacheValue) size(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return none, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.purgeExpired(c.clock())
	return starlark.MakeInt(len(kept)), nil
}
