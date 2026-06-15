# 🗃️ `cache` — bounded in-memory caches for Starlark

[![Go Reference](https://pkg.go.dev/badge/github.com/starpkg/cache.svg)](https://pkg.go.dev/github.com/starpkg/cache)
[![codecov](https://codecov.io/gh/starpkg/cache/graph/badge.svg)](https://codecov.io/gh/starpkg/cache)
![binary footprint](https://img.shields.io/badge/binary_footprint-%2B0.1_MB-blue)

Bounded, in-memory key-value caches with TTL expiry for Starlark scripts.

Each cache from `new_cache()` is an **independent namespace**. Stored values are
snapshotted through the [`serial`](https://github.com/1set/starlet) module (a
lossless `dumps`/`loads` round-trip), so a cached value is an **immutable,
independent copy** — mutating what you put in, or what you get out, never affects
the stored entry. Caches are **bounded**: once `max_entries` is reached, the
oldest entry is evicted.

A `Cache` instance exposes the methods `set`, `get`, `has`, `delete`, `clear`,
`keys`, and `size`.

## Installation

```bash
go get github.com/starpkg/cache
```

## Functions

| Function | Signature | Description |
|----------|-----------|-------------|
| `new_cache` | `new_cache(max_entries=128, ttl=0) -> Cache` | Create an independent cache. `ttl` is the default time-to-live in seconds (`0` = no expiry). |
| `Cache.set` | `Cache.set(key, value, ttl=None) -> None` | Store `value` (must be serializable). `ttl` overrides the cache default for this entry. |
| `Cache.get` | `Cache.get(key, default=None) -> value` | Return a fresh copy of the value, or `default` if missing/expired. |
| `Cache.has` | `Cache.has(key) -> bool` | Whether `key` is present and not expired. |
| `Cache.delete` | `Cache.delete(key) -> bool` | Remove `key`; returns whether it was present. |
| `Cache.clear` | `Cache.clear() -> None` | Remove all entries. |
| `Cache.keys` | `Cache.keys() -> list` | Non-expired keys, in insertion order. |
| `Cache.size` | `Cache.size() -> int` | Number of non-expired entries. |

## Usage

```python
load("cache", "new_cache")

c = new_cache(max_entries=1000, ttl=60)   # up to 1000 entries, 60s default TTL

c.set("user:42", {"name": "Ada", "tier": "pro"})
c.set("token", "abc", ttl=10)             # this entry expires in 10s

c.get("user:42")          # => {"name": "Ada", "tier": "pro"}
c.get("missing", "n/a")   # => "n/a"
c.has("token")            # => True
c.size()                  # => 2

# Values are independent snapshots:
v = [1, 2]
c.set("nums", v)
v.append(3)               # does not affect the cached value
c.get("nums")             # => [1, 2]
```

## Design & semantics

### 1. Eviction: FIFO, not LRU

When a cache grows past `max_entries`, the **oldest-inserted** entry is evicted
first (first-in, first-out). Re-`set`ting an existing key updates its value but
keeps its original insertion position, so age is measured from first insertion.

This is deliberately **not LRU**. FIFO needs no per-access bookkeeping — a `get`
never reorders anything — which keeps the implementation simple and the
behaviour predictable. The tradeoff is that a frequently read ("hot") key is not
protected from eviction the way LRU would protect it. If you need recency-aware
caching, that is out of scope for this module.

### 2. Namespacing: independent instances

Each `new_cache()` returns an **independent** cache instance. There is **no
central manager or registry** — two caches never share entries, a bound, or a
default TTL. To separate namespaces, create a separate cache per namespace.

### 3. Value snapshotting via `serial`

On `set`, the value is encoded with `serial.dumps` into an immutable string; on
`get`, it is decoded with `serial.loads` into a **fresh** value. This makes every
cached value a **lossless, immutable, independent copy**:

```python
v = [1, 2]
c.set("nums", v)
v.append(3)        # mutating the original does not touch the stored entry
c.get("nums")      # => [1, 2]
got = c.get("nums")
got.append(99)     # mutating a returned copy does not touch the stored entry
c.get("nums")      # => [1, 2]
```

Only **serializable** values can be cached. A non-serializable value (a function,
a builtin, …) is **rejected** — there is **no silent / best-effort variant**;
`set` returns the error.

### 4. Failure handling: errors, never host panics

- `set` **returns an error** (it does not panic) on a non-serializable value; the
  message contains `"serializable"` so callers can detect it.
- `get` **returns an error** (it does not panic) if a stored snapshot ever fails
  to decode.
- No operation panics the host on any path. Bad arguments surface as ordinary
  Starlark errors.

### 5. Read/write consistency

Every operation is guarded by a `sync.Mutex`, so reads and writes are
**serializable** and each call observes a consistent snapshot of the cache.
Combined with the `serial` snapshotting above, values are isolated: a reader gets
its own decoded copy, never a live reference into the store.

### 6. Limits

- `max_entries` — the FIFO bound (default `128`), host-configurable.
- `default_ttl` — the default time-to-live in seconds (default `0` = no expiry),
  host-configurable, and overridable per call via `set(..., ttl=)`.

**TTL expiry is lazy:** expired entries are purged on per-key access
(`get` / `has` / `delete`) and when `keys()` / `size()` scan, so they never
linger counting toward `max_entries`. Nothing runs in the background. A Go host
can inject a clock with `cache.NewModuleWithClock` for deterministic TTL testing.

## Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `max_entries` | `int` | `128` | Default maximum entries per cache |
| `default_ttl` | `int` | `0` | Default TTL in seconds (`0` = no expiry); seeds the `new_cache()` `ttl=` default |

Settable via `CACHE_MAX_ENTRIES` / `CACHE_DEFAULT_TTL`. A Go host can inject a
clock with `cache.NewModuleWithClock` for deterministic TTL testing.
