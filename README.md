# 🗃️ `cache` — bounded in-memory caches for Starlark

[![Go Reference](https://pkg.go.dev/badge/github.com/starpkg/cache.svg)](https://pkg.go.dev/github.com/starpkg/cache)
[![codecov](https://codecov.io/gh/starpkg/cache/graph/badge.svg)](https://codecov.io/gh/starpkg/cache)
![binary footprint](https://img.shields.io/badge/binary_footprint-%2B0.3_MB-blue)

Bounded, in-memory key-value caches with TTL expiry for Starlark scripts.

## Overview

- **Independent namespaces** — each `new_cache()` returns its own cache instance;
  there is no central manager or registry. Two caches never share entries.
- **Bounded** — a cache never holds more than `max_entries` live entries; once a
  write would exceed the bound, the oldest-inserted entry is evicted (FIFO, not
  LRU).
- **Immutable snapshots** — stored values round-trip through the
  [`serial`](https://github.com/1set/starlet) codec, so a cached value is a
  lossless, independent copy: mutating what you put in, or what you get out, never
  affects the stored entry.
- **Lazy TTL** — expired entries are purged on access (`get` / `has` / `delete`)
  and on the `keys()` / `size()` scans; nothing runs in the background.
- **Local capability** — an in-process, in-memory store. No network, no external
  service, no persistence (not a wrapper over Redis/Memcached).

For the complete per-builtin reference — signatures, parameters, returns,
errors, examples — and the configuration accessors, see
**[docs/API.md](docs/API.md)**.

## Installation

```bash
go get github.com/starpkg/cache
```

## Quickstart

```python
load("cache", "new_cache")

c = new_cache(max_entries=1000, ttl=60)   # up to 1000 entries, 60s default TTL

c.set("user:42", {"name": "Ada", "tier": "pro"})
c.set("token", "abc", ttl=10)             # this entry expires in 10s

c.get("user:42")          # => {"name": "Ada", "tier": "pro"}
c.get("missing", "n/a")   # => "n/a"
c.has("token")            # => True
c.size()                  # => 2
```

Cached values are independent snapshots:

```python
v = [1, 2]
c.set("nums", v)
v.append(3)               # does not affect the cached value
c.get("nums")             # => [1, 2]
```

## Starlark API at a glance

Top-level builtin (`load("cache", …)`):

- `new_cache(max_entries=128, ttl=0)` — create an independent cache; returns a
  `Cache` object.

`Cache` object methods:

- `set(key, value, ttl=None)` — store a (serializable) value; `ttl` overrides the
  cache default for this entry.
- `get(key, default=None)` — return a fresh copy of the value, or `default` if
  missing/expired.
- `has(key)` — whether `key` is present and not expired.
- `delete(key)` — remove `key`; returns whether it was present.
- `clear()` — remove all entries.
- `keys()` — non-expired keys, in insertion order.
- `size()` — number of non-expired entries.

See **[docs/API.md](docs/API.md)** for the full signatures, return values,
errors, and examples of every builtin and method above.

## Configuration

The module's options (`max_entries`, `default_ttl`) are configured via
environment variables (`CACHE_*`) or per-option `get_<key>` / `set_<key>`
accessor builtins, and seed the defaults for `new_cache`. A Go host can also
inject a clock with `cache.NewModuleWithClock` for deterministic TTL testing. See
the [Configuration section of docs/API.md](docs/API.md#configuration) for the
full option table, defaults, and accessors.

## License

MIT
