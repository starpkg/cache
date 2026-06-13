# 🗃️ `cache` — bounded in-memory caches for Starlark

[![Go Reference](https://pkg.go.dev/badge/github.com/starpkg/cache.svg)](https://pkg.go.dev/github.com/starpkg/cache)

Bounded, in-memory key-value caches with TTL expiry for Starlark scripts.

Each cache from `new_cache()` is an **independent namespace**. Stored values are
snapshotted through the [`serial`](https://github.com/1set/starlet) module (a
lossless `dumps`/`loads` round-trip), so a cached value is an **immutable,
independent copy** — mutating what you put in, or what you get out, never affects
the stored entry. Caches are **bounded**: once `max_entries` is reached, the
oldest entry is evicted.

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

## Notes

- **Serializable values only.** Values are stored via `serial`, so functions and
  other non-serializable values are rejected by `set` with an error.
- **Namespacing** is per-cache: create separate caches for separate namespaces.
- **Eviction** is FIFO (oldest insertion first) when `max_entries` is exceeded.

## Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `max_entries` | `int` | `128` | Default maximum entries per cache |
| `default_ttl` | `int` | `0` | Default TTL in seconds (`0` = no expiry) |

Settable via `CACHE_MAX_ENTRIES` / `CACHE_DEFAULT_TTL`. A Go host can inject a
clock with `cache.NewModuleWithClock` for deterministic TTL testing.
