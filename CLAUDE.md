# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`starpkg/cache` is an **L4 domain module** of the Star\* ecosystem: it exposes bounded, in-memory key-value caches with TTL expiry to Starlark scripts. A script loads the module, calls `new_cache()` to get an independent cache instance, then `set`/`get`/`has`/`delete`/`clear`/`keys`/`size` against it.

Where it sits in the starpkg positioning — *"support for necessary LOCAL operations + simple abstractions over common ONLINE services"* — `cache` is squarely a **local capability**: an in-process, in-memory store. There is no network, no external service, no persistence. It is the local-operations side of starpkg, the same family as a process-local scratchpad, not a wrapper over a remote cache (no Redis/Memcached client here).

It is pure Go, all platforms, no cgo, and has **no third-party SDK** — the only non-trivial dependency is the ecosystem's own `serial` codec (see Architecture). Layer position: depends downward on `starpkg/base` (the module/config system), `1set/starlet` (the Machine + `lib/serial` + `dataconv/types`), and transitively `1set/starlight` + `go.starlark.net`. Nothing in the ecosystem depends on it.

## Dev commands

Pure Go library with a Makefile. From this repo:

```bash
make test                                  # -race -cover, the working bar
make ci                                    # -race -cover profile + bench compile (what CI runs)
go test ./... -run TestCacheTTL            # a single test
make bench                                 # benchmarks
gofmt -l . && go vet ./...                 # must be clean before commit
go run github.com/1set/meta/doccov@master .   # the doc-coverage gate (exit 0 = README covers every builtin)
```

**Verify on the go floor in Docker** — this repo's floor is **go 1.19** (its go.mod, the ecosystem baseline `go.starlark.net` pin uses `maphash.String` which needs ≥1.19), newer than the local toolchain. Behavior on the floor must be checked in a container:

```bash
docker run --rm -v "$PWD":/src -v "$HOME/go/pkg/mod":/go/pkg/mod -w /src golang:1.19 go test -race -count=1 ./...
```

There are no external-service tests — everything is in-memory, so nothing self-skips on missing credentials. Integration scripts under `../test/cache/*.star` (if any) live in the **private `starpkg/test` repo** and auto-skip when that directory is absent (e.g. in CI).

## Architecture (the part that spans files)

The whole module is one file, `cache.go`, around two ideas: a thin **`Module`** (config + the `new_cache` builtin) and the **`cacheValue`** object it mints (a bounded, TTL-aware, snapshot-backed store).

- **`Module`** — wraps a `base.ConfigurableModule` plus a `base.ConfigurableModuleExt`. `NewModule()` / `NewModuleWithClock(clock)` construct it; the clock is `time.Now` by default and injectable for deterministic TTL tests. It registers two config keys — `max_entries` (default `128`) and `default_ttl` (seconds, default `0` = no expiry) — each also reachable via env (`CACHE_MAX_ENTRIES` / `CACHE_DEFAULT_TTL`). At construction it loads `1set/starlet/lib/serial` once and stashes the `dumps`/`loads` builtins, which every cache instance reuses.
- **`LoadModule()`** exposes a single builtin: **`new_cache`** (`new_cache(max_entries=<config>, ttl=<config>) -> Cache`). A non-positive `max_entries` falls back to the default; a negative `ttl` is a clean error.
- **`cacheValue`** is the `Cache` object returned by `new_cache`. It implements `starlark.Value` + `starlark.HasAttrs`. Its methods are the script-facing surface, each registered in `Attr` under a static literal name (`set`, `get`, `has`, `delete`, `clear`, `keys`, `size`) so the doccov scanner can read the surface statically. Internally it holds `entries map[string]cacheEntry`, an `order []string` insertion-order slice for FIFO eviction, a `sync.Mutex`, the clock, and the `dumps`/`loads` builtins.
- **`cacheEntry`** is one stored value: `data` (the serial-encoded immutable snapshot string) + `expiresAt` (zero == no expiry).

**Data flow of a value** — this is the one non-obvious wrap point. `cache` has no storage SDK; instead it borrows the ecosystem's own serialization codec:

- `set(key, value, ttl)` calls `serial.dumps` (via `dumps.CallInternal`) to snapshot `value` into an immutable string before storing it. A value `serial` cannot encode (a function, a builtin, …) makes `set` return a clean error containing `"serializable"` — never a panic, never a silent drop.
- `get(key, default)` calls `serial.loads` to decode the stored string into a **fresh, independent** Starlark value. So mutating what you put in, or what you get out, never touches the stored entry.

`base`/`starlet`/`starlight` dependency surface: config plumbing comes from `base`; the Machine, `serial`, and `dataconv/types.NullableInt` (used to distinguish an absent/`None` `ttl` from an explicit one) come from `starlet`; value conversion under the hood is `starlight`. There is no other third-party SDK to wrap.

## Invariants / hardening (preserve when editing)

1. **No host panics from script input.** Every method validates args via `starlark.UnpackArgs` and returns ordinary Starlark errors. A non-serializable `set` value and a corrupt stored snapshot on `get` both surface as errors, never a `panic`. Don't introduce a code path that can panic the host on script input.
2. **Lossless, independent snapshots.** Values round-trip through `serial.dumps`/`serial.loads`, so a cached value is an immutable, independent copy. Don't add a "store the live reference" fast path — isolation is a guarantee, not an optimization to skip.
3. **Bounded by `max_entries`.** A cache never holds more live entries than `max_entries`; the FIFO eviction loop in `set` drops the oldest-inserted key (`order[0]`) until the bound holds. New write paths must respect the bound.
4. **Eviction is FIFO, not LRU.** A `get` never reorders. Re-`set`ting an existing key keeps its original insertion position (age is measured from first insertion). This is deliberate — FIFO needs no per-access bookkeeping. Don't quietly turn it into LRU; that is a documented behavior change, not a tweak.
5. **TTL expiry is lazy.** Nothing runs in the background. An entry is purged on per-key access (`get`/`has`/`delete`) and on the `keys()`/`size()` scans (via `purgeExpired`), so dead entries never linger counting toward `max_entries`. Don't spawn a sweeper goroutine.
6. **Serializable under the lock.** Every method takes `mu`, so reads and writes are serializable and each call sees a consistent snapshot. Keep new methods on the same mutex.
7. **Backward compatibility (the iron rule).** Existing scripts must keep working unchanged. `new_cache()` with no args yields the historical defaults (128 entries, no TTL); `ttl=None`/absent on `set` falls back to the cache default; a negative `ttl` is rejected symmetrically with `new_cache`. Any new lever must default to the historical behavior.

## Test organization

Group by functional goal — **do not add one `*_test.go` per fix.** `cache_test.go` is the home, opened with a commented section list: basic ops via Starlark, value independence (serial snapshot), bounded eviction (incl. strict FIFO-not-LRU order), TTL expiry with an injected clock (incl. `set`'s `ttl=None`/negative handling and `size()` purging), and concurrency (`-race`). Add a new test as a **section here**, not a new file. Tests are table/script-driven; no third-party test framework. The injectable clock (`NewModuleWithClock`) is what makes TTL tests deterministic — use it instead of sleeping.

## Documentation

Three layers must stay in sync (enforced by the doc standard, `plan/starpkg文档标准（DOC-STD）`):
- **`README.md`** — every script-facing builtin and method (`new_cache`, and `set`/`get`/`has`/`delete`/`clear`/`keys`/`size`) documented as a backtick whole-word; host levers (`max_entries`, `default_ttl`, env vars, `NewModuleWithClock`) under *Design & semantics* / *Configuration*. Names and signatures must match the code.
- **GoDoc** — package comment + a doc comment whose first word is the symbol name on every exported symbol (`ModuleName`, `Module`, `NewModule`, `NewModuleWithClock`, `(*Module).LoadModule`), gated by `revive`'s `exported` rule in CI.
- **The doccov gate** — `go run github.com/1set/meta/doccov@master .` must exit 0; it statically scans `starlark.NewBuiltin("…", …)` calls and fails if any name is not a backtick word in the README. Methods are registered under static literal names in `Attr` precisely so this scanner can see them. Wired into CI via the reusable workflow's `doc-coverage: true`.

## Release discipline

- **Floor = go 1.19** (this repo's go.mod), pinned to the ecosystem baseline `go.starlark.net` (`ffb3f39`) + `1set/starlet v0.2.1` + `starpkg/base v0.1.0`. The floor only rises in this repo's own pin-upgrade PR.
- **CI matrix** = `[1.19.x, 1.25.x]` via the centralized reusable workflow in `1set/meta` (`go-ci.yml`), pinned to a commit SHA, with `doc-coverage: true`.
- **Bumping the version, the go floor, or tagging are user-confirmed actions** — never tag autonomously; the pin-upgrade PR is the last of a repo's series and must merge before any tag; default to patch bumps; published tags are immutable in the Go module proxy.
