package cache

// Tests for the cache module.
//
// Sections:
//   - basic get/set/has/delete/clear/keys/size via Starlark
//   - value independence (serial snapshot)
//   - bounded eviction
//   - TTL expiry with an injected clock (incl. set()'s ttl=None/negative
//     handling and size() purging expired entries)

import (
	"strings"
	"testing"
	"time"

	"github.com/1set/starlet"
	"go.starlark.net/starlark"
)

// newCacheVal builds a cache value directly (with the given clock) for Go-level
// testing of TTL/eviction without round-tripping through a script.
func newCacheVal(t *testing.T, clock func() time.Time, maxEntries, ttl int) (*cacheValue, *starlark.Thread) {
	t.Helper()
	mod := NewModuleWithClock(clock)
	thread := &starlark.Thread{Name: "test"}
	nc := starlark.NewBuiltin("cache.new_cache", mod.newCache)
	v, err := nc.CallInternal(thread, starlark.Tuple{starlark.MakeInt(maxEntries), starlark.MakeInt(ttl)}, nil)
	if err != nil {
		t.Fatalf("new_cache: %v", err)
	}
	return v.(*cacheValue), thread
}

func call(t *testing.T, cv *cacheValue, thread *starlark.Thread, name string, args ...starlark.Value) (starlark.Value, error) {
	t.Helper()
	fn, err := cv.Attr(name)
	if err != nil || fn == nil {
		t.Fatalf("Attr(%q) = (%v, %v)", name, fn, err)
	}
	return fn.(*starlark.Builtin).CallInternal(thread, starlark.Tuple(args), nil)
}

// --- basic operations via Starlark -------------------------------------------

func TestCacheBasic(t *testing.T) {
	script := `
load("cache", "new_cache")
c = new_cache()
c.set("a", 1)
c.set("b", [1, 2, 3])
got_a = c.get("a")
got_b = c.get("b")
missing = c.get("missing", "fallback")
has_a = c.has("a")
deleted = c.delete("a")
size_after = c.size()
`
	m := starlet.NewDefault()
	m.SetScriptContent([]byte(script))
	m.SetLazyloadModules(map[string]starlet.ModuleLoader{ModuleName: NewModule().LoadModule()})
	res, err := m.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res["got_a"] != int64(1) {
		t.Errorf("got_a = %v, want 1", res["got_a"])
	}
	if res["missing"] != "fallback" {
		t.Errorf("missing = %v, want fallback", res["missing"])
	}
	if res["has_a"] != true {
		t.Errorf("has_a = %v, want true", res["has_a"])
	}
	if res["deleted"] != true {
		t.Errorf("deleted = %v, want true", res["deleted"])
	}
	if res["size_after"] != int64(1) {
		t.Errorf("size_after = %v, want 1 (only b left)", res["size_after"])
	}
}

// --- value independence (serial snapshot) ------------------------------------

func TestCacheValueIndependence(t *testing.T) {
	// Mutating the value after caching, and mutating a got value, must not
	// affect the stored entry.
	script := `
load("cache", "new_cache")
c = new_cache()
v = [1, 2]
c.set("k", v)
v.append(3)          # mutate original after set
got1 = c.get("k")
got1.append(99)      # mutate the returned copy
got2 = c.get("k")
`
	m := starlet.NewDefault()
	m.SetScriptContent([]byte(script))
	m.SetLazyloadModules(map[string]starlet.ModuleLoader{ModuleName: NewModule().LoadModule()})
	res, err := m.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// got2 must be the pristine [1, 2] (starlet returns it as a Go slice).
	lst, ok := res["got2"].([]interface{})
	if !ok {
		t.Fatalf("got2 is %T, want slice", res["got2"])
	}
	if len(lst) != 2 {
		t.Errorf("stored value was mutated: len = %d, want 2", len(lst))
	}
}

func TestCacheRejectsNonSerializable(t *testing.T) {
	script := `
load("cache", "new_cache")
c = new_cache()
c.set("fn", new_cache)   # a builtin is not serializable
`
	m := starlet.NewDefault()
	m.SetScriptContent([]byte(script))
	m.SetLazyloadModules(map[string]starlet.ModuleLoader{ModuleName: NewModule().LoadModule()})
	_, err := m.Run()
	if err == nil || !strings.Contains(err.Error(), "serializable") {
		t.Errorf("expected not-serializable error, got %v", err)
	}
}

// --- bounded eviction --------------------------------------------------------

func TestCacheEviction(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 2, 0) // max 2, no ttl
	for _, k := range []string{"a", "b", "c"} {
		if _, err := call(t, cv, thread, "set", starlark.String(k), starlark.MakeInt(1)); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	// "a" (oldest) should have been evicted when "c" was added.
	got, _ := call(t, cv, thread, "has", starlark.String("a"))
	if got == starlark.True {
		t.Error("oldest entry a should have been evicted")
	}
	sz, _ := call(t, cv, thread, "size")
	if sz.(starlark.Int).BigInt().Int64() != 2 {
		t.Errorf("size = %v, want 2", sz)
	}
}

func TestCacheKeysAndClear(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 16, 0)
	for _, k := range []string{"a", "b"} {
		call(t, cv, thread, "set", starlark.String(k), starlark.MakeInt(1))
	}
	keys, _ := call(t, cv, thread, "keys")
	if l := keys.(*starlark.List); l.Len() != 2 {
		t.Errorf("keys len = %d, want 2", l.Len())
	}
	call(t, cv, thread, "clear")
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
		t.Errorf("size after clear = %v, want 0", sz)
	}
}

// --- TTL expiry with injected clock ------------------------------------------

func TestCacheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	cv, thread := newCacheVal(t, func() time.Time { return now }, 16, 0)

	// Set with an explicit 10s ttl.
	if _, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(42), starlark.MakeInt(10)); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Before expiry.
	if got, _ := call(t, cv, thread, "get", starlark.String("k")); got.(starlark.Int).BigInt().Int64() != 42 {
		t.Errorf("before expiry get = %v, want 42", got)
	}
	// Advance past the ttl.
	now = now.Add(11 * time.Second)
	got, _ := call(t, cv, thread, "get", starlark.String("k"), starlark.String("gone"))
	if got != starlark.String("gone") {
		t.Errorf("after expiry get = %v, want \"gone\"", got)
	}
	// And it's purged from size.
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
		t.Errorf("size after expiry = %v, want 0", sz)
	}
}

// TestCacheSetTTLArg covers the ttl argument of set(): None/absent fall back to
// the cache default, a non-negative int overrides it, and a negative int is a
// clean error (symmetry with new_cache).
func TestCacheSetTTLArg(t *testing.T) {
	now := time.Unix(1000, 0)
	// Cache default ttl = 10s.
	cv, thread := newCacheVal(t, func() time.Time { return now }, 16, 10)

	// ttl=None must use the cache default (10s), not error.
	if _, err := call(t, cv, thread, "set", starlark.String("d"), starlark.MakeInt(1), starlark.None); err != nil {
		t.Fatalf("set ttl=None: %v", err)
	}
	// Absent ttl must also use the cache default.
	if _, err := call(t, cv, thread, "set", starlark.String("a"), starlark.MakeInt(1)); err != nil {
		t.Fatalf("set absent ttl: %v", err)
	}
	// Both visible now, both gone after the default 10s window.
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 2 {
		t.Fatalf("size before expiry = %v, want 2", sz)
	}
	now = now.Add(11 * time.Second)
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
		t.Errorf("ttl=None/absent did not use the default ttl: size = %v, want 0", sz)
	}

	// A negative explicit ttl is a clean error.
	_, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(1), starlark.MakeInt(-1))
	if err == nil || !strings.Contains(err.Error(), "ttl must not be negative") {
		t.Errorf("set ttl=-1: err = %v, want a 'ttl must not be negative' error", err)
	}
}

// TestCacheSizePurgesExpired asserts that size() does not count an
// expired-but-untouched entry, and actually purges it (so it stops counting
// toward max_entries) — consistent with keys().
func TestCacheSizePurgesExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	cv, thread := newCacheVal(t, func() time.Time { return now }, 16, 0)

	// Two entries with a 10s ttl; never touched again via get/has.
	for _, k := range []string{"a", "b"} {
		if _, err := call(t, cv, thread, "set", starlark.String(k), starlark.MakeInt(1), starlark.MakeInt(10)); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 2 {
		t.Fatalf("size before expiry = %v, want 2", sz)
	}

	// Advance past the ttl; entries are now expired but untouched.
	now = now.Add(11 * time.Second)
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
		t.Errorf("size after expiry = %v, want 0", sz)
	}

	// size() must have purged them outright (not merely skipped counting), so
	// the backing maps/order no longer hold the dead entries.
	cv.mu.Lock()
	nEntries, nOrder := len(cv.entries), len(cv.order)
	cv.mu.Unlock()
	if nEntries != 0 || nOrder != 0 {
		t.Errorf("size() left dead entries: entries=%d order=%d, want 0/0", nEntries, nOrder)
	}
}
