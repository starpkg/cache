# `cache` — Starlark API Reference

The complete reference for every script-facing builtin, `Cache` object method, and
configuration accessor exposed by the `cache` module. For an overview,
installation, and a quickstart, see the [README](../README.md).

The module exposes a single top-level builtin via `load("cache", …)` —
`new_cache` — plus a set of configuration accessors (`get_<key>` / `set_<key>`)
generated from the module's options. `new_cache` mints an independent `Cache`
object that carries the store methods (`set`, `get`, `has`, `delete`, `clear`,
`keys`, `size`).

Every cache is an **independent namespace**, **bounded** by `max_entries` (FIFO
eviction once the bound is exceeded), and stores values as **lossless, immutable,
independent snapshots** taken through the
[`serial`](https://github.com/1set/starlet) codec — mutating what you put in, or
what you get out, never affects the stored entry. TTL expiry is **lazy** (purged
on access, never by a background goroutine).

## Contents

- [Cache creation](#cache-creation)
- [Cache object methods](#cache-object-methods)
  - [`set(key, value, ttl=None)`](#setkey-value-ttlnone)
  - [`get(key, default=None)`](#getkey-defaultnone)
  - [`has(key)`](#haskey)
  - [`delete(key)`](#deletekey)
  - [`clear()`](#clear)
  - [`keys()`](#keys)
  - [`size()`](#size)
- [Design & semantics](#design--semantics)
- [Configuration](#configuration)

## Cache creation

### `new_cache(max_entries=128, ttl=0)`

Creates a new **independent** in-memory cache instance. There is no central
manager or registry — two caches never share entries, a bound, or a default TTL;
to separate namespaces, create a separate cache per namespace.

**Parameters:**

- `max_entries` (int): Maximum number of live entries this cache holds before
  FIFO eviction kicks in (default: the module's `max_entries` config option,
  `128`). A non-positive value falls back to the built-in default of `128`.
- `ttl` (int): Default time-to-live in seconds applied to entries that do not
  override it on `set` (default: the module's `default_ttl` config option, `0`).
  `0` means no expiry. A negative value is an error.

**Returns:** A `Cache` object.

**Errors:** Fails if `ttl` is negative.

**Example:**

```python
load("cache", "new_cache")

# Up to 1000 entries, 60s default TTL
c = new_cache(max_entries=1000, ttl=60)

# Defaults: 128 entries, no expiry
d = new_cache()
```

## Cache object methods

A `Cache` object (returned by `new_cache`) exposes the methods below. Every
operation is guarded by an internal mutex, so reads and writes are serializable
and each call observes a consistent snapshot of the cache.

### `set(key, value, ttl=None)`

Stores `value` under `key`. The value is snapshotted via `serial.dumps` into an
immutable string before it is stored, so the stored entry is independent of the
caller's value.

**Parameters:**

- `key` (string): The entry key.
- `value`: The value to store. It **must be serializable** by the `serial` codec.
  A non-serializable value (a function, a builtin, …) is rejected — there is no
  silent / best-effort variant.
- `ttl` (int, optional): Per-entry time-to-live in seconds.
  - `None` or absent: use the cache's default TTL.
  - A non-negative int: that per-entry TTL in seconds (`0` = no expiry).
  - A negative int: an error (symmetry with `new_cache`).

**Returns:** `None`.

**Errors:** Returns an error (it does not panic) if `value` is not serializable;
the message contains `"serializable"` so callers can detect it. Also errors on a
negative or out-of-range `ttl`.

**Eviction:** After writing, while the cache holds more than `max_entries` live
entries, the oldest-inserted key is evicted (FIFO). Re-`set`ting an existing key
updates its value but keeps its original insertion position, so age is measured
from first insertion.

**Example:**

```python
c.set("user:42", {"name": "Ada", "tier": "pro"})
c.set("token", "abc", ttl=10)   # this entry expires in 10s

# Values are independent snapshots — mutating the original does not
# touch the stored entry.
v = [1, 2]
c.set("nums", v)
v.append(3)
c.get("nums")                   # => [1, 2]
```

### `get(key, default=None)`

Returns the value stored under `key`, or `default` if the key is missing or
expired. A live entry is decoded with `serial.loads` into a **fresh, independent**
value, so mutating the returned value never touches the stored snapshot.

**Parameters:**

- `key` (string): The entry key.
- `default` (optional): The value returned when the key is missing or expired
  (default: `None`).

**Returns:** A fresh copy of the stored value, or `default`.

**Errors:** Returns an error (it does not panic) if a stored snapshot ever fails
to decode.

**Note:** A lookup is read-only with respect to FIFO order — `get` never
reorders. The only mutation `get` performs is lazily purging the key if it has
expired.

**Example:**

```python
c.set("user:42", {"name": "Ada", "tier": "pro"})

c.get("user:42")          # => {"name": "Ada", "tier": "pro"}
c.get("missing", "n/a")   # => "n/a"

# A returned value is an independent copy:
got = c.get("user:42")
got["tier"] = "free"      # does not touch the stored entry
c.get("user:42")          # => {"name": "Ada", "tier": "pro"}
```

### `has(key)`

Reports whether `key` is present and not expired. Lazily purges the key if it has
expired.

**Parameters:**

- `key` (string): The entry key.

**Returns:** A `bool`.

**Example:**

```python
c.set("token", "abc", ttl=10)
c.has("token")     # => True
c.has("missing")   # => False
```

### `delete(key)`

Removes `key` from the cache.

**Parameters:**

- `key` (string): The entry key.

**Returns:** A `bool` — whether the key was present.

**Example:**

```python
c.set("token", "abc")
c.delete("token")   # => True
c.delete("token")   # => False (already gone)
```

### `clear()`

Removes all entries from the cache.

**Parameters:** None.

**Returns:** `None`.

**Example:**

```python
c.clear()
c.size()   # => 0
```

### `keys()`

Returns the non-expired keys in **insertion order**. Expired entries are lazily
purged while scanning, so they never linger counting toward `max_entries`.

**Parameters:** None.

**Returns:** A `list` of keys (strings).

**Example:**

```python
c.set("a", 1)
c.set("b", 2)
c.keys()   # => ["a", "b"]
```

### `size()`

Returns the number of non-expired entries. Like `keys()`, it lazily purges
expired entries while scanning.

**Parameters:** None.

**Returns:** An `int`.

**Example:**

```python
c.set("a", 1)
c.set("b", 2)
c.size()   # => 2
```

## Design & semantics

### Eviction: FIFO, not LRU

When a cache grows past `max_entries`, the **oldest-inserted** entry is evicted
first (first-in, first-out). Re-`set`ting an existing key updates its value but
keeps its original insertion position, so age is measured from first insertion.

This is deliberately **not LRU**. FIFO needs no per-access bookkeeping — a `get`
never reorders anything — which keeps the implementation simple and the
behaviour predictable. The tradeoff is that a frequently read ("hot") key is not
protected from eviction the way LRU would protect it. If you need recency-aware
caching, that is out of scope for this module.

### Namespacing: independent instances

Each `new_cache()` returns an **independent** cache instance. There is **no
central manager or registry** — two caches never share entries, a bound, or a
default TTL. To separate namespaces, create a separate cache per namespace.

### Value snapshotting via `serial`

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

### Failure handling: errors, never host panics

- `set` **returns an error** (it does not panic) on a non-serializable value; the
  message contains `"serializable"` so callers can detect it.
- `get` **returns an error** (it does not panic) if a stored snapshot ever fails
  to decode.
- No operation panics the host on any path. Bad arguments surface as ordinary
  Starlark errors.

### TTL expiry is lazy

Expired entries are purged on per-key access (`get` / `has` / `delete`) and when
`keys()` / `size()` scan, so they never linger counting toward `max_entries`.
Nothing runs in the background. A Go host can inject a clock with
`cache.NewModuleWithClock` for deterministic TTL testing.

## Configuration

Each module configuration option is exposed to scripts as a pair of generated
accessor builtins (loaded from the `cache` module alongside `new_cache`):

- **`get_<key>()`** — returns the current value of the option.
- **`set_<key>(value)`** — sets the option (returns `None`).

An option's value resolves in priority order: an explicit `set_<key>` value, the
environment variable, then the default. These options seed the defaults for
`new_cache` when the corresponding argument is not provided.

None of the `cache` options are secret, so every option exposes **both**
`get_<key>` and `set_<key>`. (A secret option would expose only its `set_<key>`
accessor — never a getter — but this module has none.)

| Option | Getter | Setter | Type | Env var | Default | Description |
|--------|--------|--------|------|---------|---------|-------------|
| `max_entries` | `get_max_entries` | `set_max_entries` | int | `CACHE_MAX_ENTRIES` | `128` | Default maximum entries per cache (seeds the `new_cache()` `max_entries=` default; a non-positive value falls back to `128`) |
| `default_ttl` | `get_default_ttl` | `set_default_ttl` | int | `CACHE_DEFAULT_TTL` | `0` | Default time-to-live in seconds (`0` = no expiry); seeds the `new_cache()` `ttl=` default |

**Example:**

```python
load(
    "cache",
    "new_cache",
    # getters
    "get_max_entries", "get_default_ttl",
    # setters
    "set_max_entries", "set_default_ttl",
)

set_max_entries(1000)
set_default_ttl(60)
print(get_max_entries())   # 1000

c = new_cache()            # up to 1000 entries, 60s default TTL
```

### Host clock injection (Go side)

A Go host can inject a clock with `cache.NewModuleWithClock(clock)` so TTL expiry
is deterministic in tests. `cache.NewModule()` uses the wall clock (`time.Now`).
This is a host-side construction lever, not a script-facing accessor.
