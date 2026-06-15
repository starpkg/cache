package cache

// Tests for the cache module.
//
// Sections:
//   - basic get/set/has/delete/clear/keys/size via Starlark
//   - value independence (serial snapshot)
//   - bounded eviction (incl. strict FIFO — not LRU — order)
//   - TTL expiry with an injected clock (incl. set()'s ttl=None/negative
//     handling and size() purging expired entries)
//   - concurrency: concurrent get/set is mutex-safe under -race
//   - new_cache config defaulting & validation (max_entries fallback, negative
//     ttl, out-of-range ints)
//   - argument validation: every builtin rejects bad args with a clean Starlark
//     error and never panics the host
//   - serialization hardening: non-serializable values error cleanly (function,
//     builtin, struct, non-finite float, reference cycle) and serializable
//     values round-trip losslessly (dict/tuple/set/bytes/big int)
//   - the starlark.Value / HasAttrs surface (String/Type/Freeze/Truth/Hash/
//     AttrNames/Attr) of the Cache object

import (
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1set/starlet"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// runScript runs a Starlark script with the cache module loaded (wall clock).
func runScript(t *testing.T, script string) (map[string]interface{}, error) {
	t.Helper()
	m := starlet.NewDefault()
	m.SetScriptContent([]byte(script))
	m.SetLazyloadModules(map[string]starlet.ModuleLoader{ModuleName: NewModule().LoadModule()})
	return m.Run()
}

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

// TestCacheEvictionIsFIFO pins down the documented policy: eviction is FIFO
// (oldest *inserted* first), NOT LRU. We insert a, b, c into a cache of max 2,
// then read "a" (which under LRU would protect it), then insert "d". FIFO must
// still evict by insertion age, so after b,c,d the survivors are c and d — "a"
// and "b" are gone, and reading "a" did not save it.
func TestCacheEvictionIsFIFO(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 2, 0) // max 2, no ttl

	mustSet := func(k string) {
		if _, err := call(t, cv, thread, "set", starlark.String(k), starlark.MakeInt(1)); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	mustSet("a")
	mustSet("b") // store now: a, b

	// Read "a": under LRU this would mark it most-recently-used and protect it.
	if _, err := call(t, cv, thread, "get", starlark.String("a")); err != nil {
		t.Fatalf("get a: %v", err)
	}

	mustSet("c") // FIFO evicts the oldest insert, "a" -> store: b, c
	mustSet("d") // FIFO evicts "b"            -> store: c, d

	present := func(k string) bool {
		got, _ := call(t, cv, thread, "has", starlark.String(k))
		return got == starlark.True
	}
	if present("a") {
		t.Error("FIFO violated: reading 'a' protected it (that would be LRU)")
	}
	if present("b") {
		t.Error("FIFO violated: 'b' should have been evicted by 'd'")
	}
	if !present("c") || !present("d") {
		t.Error("FIFO violated: 'c' and 'd' (newest) should survive")
	}

	// keys() should list survivors in insertion order: c then d.
	keys, _ := call(t, cv, thread, "keys")
	lst := keys.(*starlark.List)
	if lst.Len() != 2 ||
		lst.Index(0) != starlark.String("c") ||
		lst.Index(1) != starlark.String("d") {
		t.Errorf("keys = %v, want [c d] in insertion order", keys)
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

// TestCacheHasAndGetPurgeExpired covers the lazy per-key purge on has() and
// get(): an entry that has expired but was never re-touched must read as absent
// AND be physically removed from the backing store (so it stops counting toward
// max_entries), consistent with the keys()/size() scans.
func TestCacheHasAndGetPurgeExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	cv, thread := newCacheVal(t, func() time.Time { return now }, 16, 0)

	if _, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(1), starlark.MakeInt(10)); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Live before expiry.
	if got, _ := call(t, cv, thread, "has", starlark.String("k")); got != starlark.True {
		t.Fatalf("has before expiry = %v, want True", got)
	}

	// Advance past the ttl, then probe via has() (not size()/get()).
	now = now.Add(11 * time.Second)
	if got, _ := call(t, cv, thread, "has", starlark.String("k")); got != starlark.False {
		t.Errorf("has after expiry = %v, want False", got)
	}
	// has() must have physically purged it, not merely reported false.
	cv.mu.Lock()
	nEntries, nOrder := len(cv.entries), len(cv.order)
	cv.mu.Unlock()
	if nEntries != 0 || nOrder != 0 {
		t.Errorf("has() left a dead entry: entries=%d order=%d, want 0/0", nEntries, nOrder)
	}

	// Same for get(): a fresh expired entry is purged on lookup.
	if _, err := call(t, cv, thread, "set", starlark.String("g"), starlark.MakeInt(1), starlark.MakeInt(10)); err != nil {
		t.Fatalf("set g: %v", err)
	}
	now = now.Add(11 * time.Second)
	if got, _ := call(t, cv, thread, "get", starlark.String("g"), starlark.String("gone")); got != starlark.String("gone") {
		t.Errorf("get after expiry = %v, want gone", got)
	}
	cv.mu.Lock()
	nEntries, nOrder = len(cv.entries), len(cv.order)
	cv.mu.Unlock()
	if nEntries != 0 || nOrder != 0 {
		t.Errorf("get() left a dead entry: entries=%d order=%d, want 0/0", nEntries, nOrder)
	}
}

// --- concurrency -------------------------------------------------------------

// TestCacheConcurrentAccess hammers a single cache from many goroutines doing
// interleaved set/get/has/size/keys/delete. It asserts no operation errors out;
// its real purpose is to be run under `-race`, where the sync.Mutex must keep
// the shared entries/order maps free of data races.
func TestCacheConcurrentAccess(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, _ := newCacheVal(t, clock, 64, 0)

	const goroutines = 16
	const iters = 200

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine needs its own thread; Starlark threads are not
			// safe to share across goroutines.
			thread := &starlark.Thread{Name: fmt.Sprintf("worker-%d", g)}
			fail := func(err error) { errs <- err }
			cset, _ := cv.Attr("set")
			cget, _ := cv.Attr("get")
			chas, _ := cv.Attr("has")
			cdel, _ := cv.Attr("delete")
			csize, _ := cv.Attr("size")
			ckeys, _ := cv.Attr("keys")
			for i := 0; i < iters; i++ {
				key := starlark.String(fmt.Sprintf("k%d", i%8))
				if _, err := cset.(*starlark.Builtin).CallInternal(thread, starlark.Tuple{key, starlark.MakeInt(i)}, nil); err != nil {
					fail(err)
					return
				}
				if _, err := cget.(*starlark.Builtin).CallInternal(thread, starlark.Tuple{key}, nil); err != nil {
					fail(err)
					return
				}
				if _, err := chas.(*starlark.Builtin).CallInternal(thread, starlark.Tuple{key}, nil); err != nil {
					fail(err)
					return
				}
				if _, err := csize.(*starlark.Builtin).CallInternal(thread, nil, nil); err != nil {
					fail(err)
					return
				}
				if _, err := ckeys.(*starlark.Builtin).CallInternal(thread, nil, nil); err != nil {
					fail(err)
					return
				}
				if i%5 == 0 {
					if _, err := cdel.(*starlark.Builtin).CallInternal(thread, starlark.Tuple{key}, nil); err != nil {
						fail(err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent op failed: %v", err)
	}
}

// --- new_cache config defaulting & validation --------------------------------

// TestNewCacheConfigDefaulting covers new_cache()'s own argument handling: a
// non-positive max_entries falls back to the historical default (128), a
// negative ttl is a clean error, and an out-of-int64-range max_entries surfaces
// as an UnpackArgs error rather than a panic.
func TestNewCacheConfigDefaulting(t *testing.T) {
	mod := NewModuleWithClock(func() time.Time { return time.Unix(1000, 0) })
	thread := &starlark.Thread{Name: "test"}
	nc := starlark.NewBuiltin("cache.new_cache", mod.newCache)

	mk := func(args ...starlark.Value) (starlark.Value, error) {
		return nc.CallInternal(thread, starlark.Tuple(args), nil)
	}

	// No args => historical defaults (128 entries, no TTL).
	v, err := mk()
	if err != nil {
		t.Fatalf("new_cache(): %v", err)
	}
	if cv := v.(*cacheValue); cv.maxEntries != defaultMaxEntries {
		t.Errorf("default max_entries = %d, want %d", cv.maxEntries, defaultMaxEntries)
	}

	// A non-positive max_entries falls back to the default (not 0, not negative).
	for _, bad := range []int64{0, -1, -100} {
		v, err := mk(starlark.MakeInt64(bad), starlark.MakeInt(0))
		if err != nil {
			t.Fatalf("new_cache(max_entries=%d): %v", bad, err)
		}
		if cv := v.(*cacheValue); cv.maxEntries != defaultMaxEntries {
			t.Errorf("max_entries=%d should fall back to %d, got %d", bad, defaultMaxEntries, cv.maxEntries)
		}
	}

	// A positive max_entries is honoured.
	if v, err := mk(starlark.MakeInt(7), starlark.MakeInt(0)); err != nil {
		t.Fatalf("new_cache(max_entries=7): %v", err)
	} else if cv := v.(*cacheValue); cv.maxEntries != 7 {
		t.Errorf("max_entries=7 honoured? got %d", cv.maxEntries)
	}

	// A negative ttl is rejected with a clean error.
	if _, err := mk(starlark.MakeInt(8), starlark.MakeInt(-1)); err == nil ||
		!strings.Contains(err.Error(), "ttl must not be negative") {
		t.Errorf("new_cache(ttl=-1): err = %v, want 'ttl must not be negative'", err)
	}

	// An out-of-int64-range max_entries is a clean UnpackArgs error (no panic).
	huge := starlark.MakeBigInt(mustBig("99999999999999999999999999999"))
	if _, err := mk(huge); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("new_cache(huge): err = %v, want an 'out of range' error", err)
	}
}

// TestNewCacheTTLSeedsSetDefault verifies the backward-compat wiring: the
// default_ttl config seeds new_cache()'s ttl, which in turn becomes the default
// for set() entries that pass ttl=None. (Config defaulting through the module.)
func TestNewCacheTTLSeedsSetDefault(t *testing.T) {
	now := time.Unix(1000, 0)
	mod := NewModuleWithClock(func() time.Time { return now })
	thread := &starlark.Thread{Name: "test"}
	nc := starlark.NewBuiltin("cache.new_cache", mod.newCache)

	// new_cache(ttl=5) => entries set without an explicit ttl expire after 5s.
	v, err := nc.CallInternal(thread, starlark.Tuple{starlark.MakeInt(16), starlark.MakeInt(5)}, nil)
	if err != nil {
		t.Fatalf("new_cache: %v", err)
	}
	cv := v.(*cacheValue)
	if _, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(1)); err != nil {
		t.Fatalf("set: %v", err)
	}
	now = now.Add(6 * time.Second)
	if got, _ := call(t, cv, thread, "get", starlark.String("k"), starlark.String("gone")); got != starlark.String("gone") {
		t.Errorf("new_cache(ttl=5) did not seed set()'s default ttl: get = %v, want gone", got)
	}
}

// --- argument validation: clean errors, never a host panic -------------------

// TestArgValidation drives every builtin with malformed arguments and asserts a
// clean Starlark error (not a panic, not a silent success). This exercises the
// UnpackArgs error branch of each method — the first line of the "no host panic"
// invariant.
func TestArgValidation(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 16, 0)

	tests := []struct {
		name string
		fn   string
		args []starlark.Value
		want string // substring expected in the error
	}{
		// Wrong arg types.
		{"set key not string", "set", []starlark.Value{starlark.MakeInt(1), starlark.MakeInt(1)}, "set"},
		{"get key not string", "get", []starlark.Value{starlark.MakeInt(1)}, "get"},
		{"has key not string", "has", []starlark.Value{starlark.MakeInt(1)}, "has"},
		{"delete key not string", "delete", []starlark.Value{starlark.MakeInt(1)}, "delete"},
		// Missing required args.
		{"set missing value", "set", []starlark.Value{starlark.String("k")}, "set"},
		{"set missing all", "set", nil, "set"},
		{"get missing key", "get", nil, "get"},
		{"has missing key", "has", nil, "has"},
		{"delete missing key", "delete", nil, "delete"},
		// Too many args (no-arg methods must reject extras).
		{"clear extra arg", "clear", []starlark.Value{starlark.MakeInt(1)}, "clear"},
		{"keys extra arg", "keys", []starlark.Value{starlark.MakeInt(1)}, "keys"},
		{"size extra arg", "size", []starlark.Value{starlark.MakeInt(1)}, "size"},
		{"has extra arg", "has", []starlark.Value{starlark.String("k"), starlark.String("x")}, "has"},
		// ttl wrong type on set (NullableInt rejects a string).
		{"set ttl wrong type", "set", []starlark.Value{starlark.String("k"), starlark.MakeInt(1), starlark.String("nope")}, "ttl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, cv, thread, tc.fn, tc.args...)
			if err == nil {
				t.Fatalf("%s(%v): expected an error, got nil", tc.fn, tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s error = %q, want substring %q", tc.fn, err.Error(), tc.want)
			}
		})
	}
}

// TestSetTTLOutOfRange covers the reachable "ttl out of range" arm of set(): a
// ttl int that does not fit int64 is a clean error, never a panic or overflow.
func TestSetTTLOutOfRange(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 16, 0)

	huge := starlark.MakeBigInt(mustBig("99999999999999999999999999999"))
	_, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(1), huge)
	if err == nil || !strings.Contains(err.Error(), "ttl out of range") {
		t.Errorf("set(ttl=huge): err = %v, want 'ttl out of range'", err)
	}
	// The cache must not have stored anything on the error path.
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
		t.Errorf("failed set should not store: size = %v, want 0", sz)
	}
}

// --- serialization hardening: clean errors, lossless round-trip --------------

// TestSetRejectsNonSerializableValues asserts that every non-serializable value
// makes set() return a clean error whose message contains "serializable" — and
// crucially that NONE of them panic the host (invariant 1). It probes a
// function, a builtin, a host struct, non-finite floats, and a reference cycle.
func TestSetRejectsNonSerializableValues(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }

	// Script-reachable cases (function/builtin/non-finite/cycle).
	scriptCases := map[string]string{
		"function":  `c.set("k", lambda x: x)`,
		"builtin":   `c.set("k", c.set)`,
		"nan":       `c.set("k", float("nan"))`,
		"plus_inf":  `c.set("k", float("inf"))`,
		"minus_inf": `c.set("k", float("-inf"))`,
		"list_cycle": `x = []
x.append(x)
c.set("k", x)`,
	}
	for name, body := range scriptCases {
		t.Run(name, func(t *testing.T) {
			script := "load(\"cache\", \"new_cache\")\nc = new_cache()\n" + body
			_, err := runScript(t, script)
			if err == nil {
				t.Fatalf("expected an error for %s, got nil", name)
			}
			if !strings.Contains(err.Error(), "serializable") {
				t.Errorf("%s error = %q, want it to mention 'serializable'", name, err.Error())
			}
		})
	}

	// A host struct value (not constructible in the default dialect) — passed
	// directly at the Go level to exercise the struct-rejection path.
	t.Run("struct", func(t *testing.T) {
		cv, thread := newCacheVal(t, clock, 16, 0)
		st := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{"a": starlark.MakeInt(1)})
		_, err := call(t, cv, thread, "set", starlark.String("k"), st)
		if err == nil || !strings.Contains(err.Error(), "serializable") {
			t.Errorf("struct set: err = %v, want a 'serializable' error", err)
		}
		// Nothing stored on the failure path.
		if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 0 {
			t.Errorf("rejected struct should not store: size = %v, want 0", sz)
		}
	})
}

// TestRoundTripFidelity verifies invariant 2 (lossless, independent snapshots)
// across the value shapes serial supports: a nested dict, a tuple, a set, bytes,
// and a big int that does not fit int64. Each must come back equal in value.
func TestRoundTripFidelity(t *testing.T) {
	script := `
load("cache", "new_cache")
c = new_cache()
c.set("dict", {"name": "Ada", "nums": [1, 2, 3], "nil": None, "f": 1.5})
c.set("tuple", (1, "two", 3.0))
c.set("set", set([3, 1, 2]))
c.set("bytes", b"\x00\x01hi")
c.set("big", 99999999999999999999999999999)
c.set("neg_big", -99999999999999999999999999999)

g_dict = c.get("dict")
dict_ok = g_dict == {"name": "Ada", "nums": [1, 2, 3], "nil": None, "f": 1.5}
tuple_ok = c.get("tuple") == (1, "two", 3.0)
set_ok = c.get("set") == set([1, 2, 3])
bytes_ok = c.get("bytes") == b"\x00\x01hi"
big_ok = c.get("big") == 99999999999999999999999999999
neg_big_ok = c.get("neg_big") == -99999999999999999999999999999
`
	res, err := runScript(t, script)
	if err != nil {
		t.Fatalf("round-trip script: %v", err)
	}
	for _, k := range []string{"dict_ok", "tuple_ok", "set_ok", "bytes_ok", "big_ok", "neg_big_ok"} {
		if res[k] != true {
			t.Errorf("%s = %v, want true (value did not round-trip losslessly)", k, res[k])
		}
	}
}

// --- the starlark.Value / HasAttrs surface of the Cache object ---------------

// TestCacheValueSurface covers the small starlark.Value / HasAttrs surface of a
// Cache object: String/Type/Truth/Freeze/Hash and AttrNames/Attr. Hash must be a
// clean error (unhashable), Freeze must be a no-op (the store stays mutable), and
// an unknown attribute must yield (nil, nil) per the HasAttrs contract.
func TestCacheValueSurface(t *testing.T) {
	clock := func() time.Time { return time.Unix(1000, 0) }
	cv, thread := newCacheVal(t, clock, 16, 0)

	if got := cv.Type(); got != "cache.Cache" {
		t.Errorf("Type() = %q, want cache.Cache", got)
	}
	if got := cv.String(); got != "<cache.Cache max=16>" {
		t.Errorf("String() = %q, want <cache.Cache max=16>", got)
	}
	if cv.Truth() != starlark.True {
		t.Errorf("Truth() = %v, want True", cv.Truth())
	}
	if _, err := cv.Hash(); err == nil || !strings.Contains(err.Error(), "unhashable") {
		t.Errorf("Hash() err = %v, want an 'unhashable' error", err)
	}

	want := []string{"set", "get", "has", "delete", "clear", "keys", "size"}
	got := cv.AttrNames()
	if len(got) != len(want) {
		t.Fatalf("AttrNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AttrNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Every advertised name resolves to a builtin.
	for _, n := range want {
		v, err := cv.Attr(n)
		if err != nil || v == nil {
			t.Errorf("Attr(%q) = (%v, %v), want a builtin", n, v, err)
		}
	}
	// An unknown attribute is (nil, nil) per the HasAttrs contract.
	if v, err := cv.Attr("nope"); v != nil || err != nil {
		t.Errorf("Attr(\"nope\") = (%v, %v), want (nil, nil)", v, err)
	}

	// Freeze is a no-op: a frozen Cache must still accept writes (the stored
	// values are immutable snapshots, so the object itself stays usable).
	cv.Freeze()
	if _, err := call(t, cv, thread, "set", starlark.String("k"), starlark.MakeInt(1)); err != nil {
		t.Errorf("set after Freeze: %v", err)
	}
	if sz, _ := call(t, cv, thread, "size"); sz.(starlark.Int).BigInt().Int64() != 1 {
		t.Errorf("size after Freeze+set = %v, want 1", sz)
	}
}

// mustBig parses a base-10 big.Int for tests, failing the build path loudly on a
// bad literal.
func mustBig(s string) *big.Int {
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("mustBig: invalid literal " + s)
	}
	return bi
}
