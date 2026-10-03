# Concurrency Model

## Goroutines

| Goroutine | Lifetime | Role |
|---|---|---|
| `http.Server` per-connection goroutines | per connection | Run `Handler.ServeHTTP` concurrently |
| Health loop (`HealthChecker.Run`) | process lifetime until shutdown | Ticks and starts rounds |
| Probe goroutines (in `CheckAll`) | one round | One per backend; joined by a `WaitGroup` before the round ends |
| Signal-handling goroutine | until first signal | Restores default signal behavior after the first signal |

Only the health loop outlives a request, and `Server.Run` joins it with a `WaitGroup` after cancelling its context.

## Shared state, writers, readers, protection

| State | Writers | Readers | Protection |
|---|---|---|---|
| `Pool.backends` slice | construction only | every request, health checker | none needed: immutable after `NewPool` |
| `Pool.next` cursor | every request | every request | `atomic.Uint64` |
| `Backend.alive` | health transitions (under mu) | every request | `atomic.Bool`, writes also under `mu` |
| failure/success counters, last error, last change | health checker, proxy error path | `Status()` | `sync.Mutex` (`Backend.mu`) |
| `Backend.requests`, proxy totals | every request | metrics | atomics |
| `Handler.proxies` map | construction only | every request | none needed: read-only after construction |

## Why this synchronization is correct

The hot path (selection) reads `alive` and increments `next`. Neither needs a lock: a single atomic load or add is a complete, indivisible operation on a single word.

Health transitions are multi-field updates (streak counter, alive flag, last error). Those must be consistent with each other, so they happen under `Backend.mu`. The alive flag is also stored atomically so readers on the hot path never touch the mutex. Writers hold the mutex, so two concurrent writers cannot both decide "I caused the transition"; exactly one does, and only that one logs it.

No lock is ever held while performing network I/O or while a request is in flight. Logging happens after the mutex is released.

## Mutex or atomic?

The cursor is a single integer incremented independently of other state, so an atomic is the natural fit and cheaper than a mutex. A mutex would also be correct but would serialize every request through one lock. The health state spans several fields that must change together, so it uses a mutex with an atomic mirror of the one field read on the hot path. Atomics everywhere would be wrong for the multi-field part: individually atomic fields can be observed in inconsistent combinations.

## Benign race: count then walk

`Next` counts healthy backends, then walks to the target-th one. Health can change between the two passes. Consequences: a request may be routed to a backend that was ejected a microsecond ago (unavoidable in any design; the request just fails and is reported), or the walk finds fewer backends than counted, in which case the last healthy one seen is returned (or nil if none remain). There is no out-of-range access, no data race (every read is atomic), and no incorrect state. `TestSelectionWhileHealthFlips` exercises this under `-race`.

## What would go wrong without synchronization

- Plain `int` cursor with `idx++`: a read-modify-write. Two requests read the same value, both pick the same backend, and one increment is lost. Distribution skews, and the race detector flags it.
- Plain `bool` alive flag: the health goroutine writes while handlers read. Go gives no guarantee when the reader observes the write, and the compiler may cache the value in a register. The race detector flags it.
- Unprotected counters: `failures++` from the probe goroutine and the proxy error handler can lose updates, so a backend might never reach its failure threshold.
- Mutating the backend slice while handlers range over it: concurrent slice growth and iteration is a data race and can read stale or torn slice headers. This is why membership is immutable here.
- One giant mutex around the request: correct but turns the balancer into a single-threaded proxy, because the lock would be held for the entire backend round trip.

## Contention points

The only globally shared write on the hot path is the `next` counter, a single cache line contended by every request (cache-line bouncing, not blocking). At very high request rates that can show up in profiles; remedies are sharding the counter per CPU or choosing randomly (power of two choices). Per-backend `requests` counters are contended only among requests to the same backend.

## Shutdown coordination

Shutdown uses context cancellation and a `WaitGroup`: cancel the health context, call `http.Server.Shutdown` (stops listeners, waits for active handlers), then wait for the health goroutine. See `Server.Run`.
