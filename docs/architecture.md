# Architecture

## Components

| Component | Package | Responsibility |
|---|---|---|
| Backend | `balancer` | Identity (URL) and health state of one upstream. Knows nothing about forwarding or selection. |
| Pool | `balancer` | Fixed list of backends and the round-robin cursor. Answers "who gets the next request?" |
| HealthChecker | `balancer` | Probes backends and reports results to each Backend. |
| Handler | `proxy` | Forwards a request to the chosen backend with `ReverseProxy`; maps transport errors to HTTP statuses. |
| Server | `server` | Wires everything, owns the listener and the lifecycle (startup check, shutdown). |
| Config | `config` | Defaults, file, environment. |

## Why responsibilities are separated

Problem: In many small proxies one struct holds the URL, an alive flag, a reverse proxy, and selection logic, so health logic and forwarding logic change for the same reasons and are tested together.

Decision: Backend holds identity and health; Pool holds selection; Handler holds forwarding; Server holds lifecycle.

Reasoning: Each can change independently. A new algorithm (least-connections) touches Pool only. Changing timeouts or adding retries touches proxy only. The health checker can be tested with no proxy at all, and the proxy with no health checker.

Trade-off: Slightly more indirection: the proxy keeps a map from Backend to its ReverseProxy instead of a field on Backend.

Production Evolution: Larger proxies (Envoy) formalize the same split into clusters, load-balancing policies, health checkers, and filters.

## Data flow

```
Client --HTTP--> Server mux --/health,/metrics--> local handlers
                      |
                      +--everything else--> proxy.Handler
                                               |
                                    balancer.Pool.Next()  (atomic ticket, read health flags)
                                               |
                                    ReverseProxy[backend] --HTTP--> Backend
                                               |
                                    response streamed to Client
```

## Request lifecycle

1. `http.Server` accepts the connection and parses the request in its own goroutine.
2. `Handler.ServeHTTP` increments the total counter and calls `Pool.Next()`.
3. If nil: respond 503.
4. Otherwise increment the backend request counter, log method, path and backend (never query or headers), and call that backend's `ReverseProxy`.
5. `ReverseProxy` clones the request, runs `Rewrite`, sends it through the shared `http.Transport`, and copies the response.
6. On transport error, `ErrorHandler` runs (see failure lifecycle).

No lock is held at any point during steps 4 to 6.

## Health-check lifecycle

`HealthChecker.Run` is one goroutine. On each tick it calls `CheckAll`, which spawns one goroutine per backend, each bounded by the probe timeout, and waits for all of them. Rounds never overlap. When the context is cancelled `Run` returns, and `Server.Run` waits for it with a `WaitGroup`, so no goroutine outlives shutdown.

## Failure lifecycle

```
health probe fails (or proxied request hits a transport error)
        |
        v
Backend.RecordFailure: consecutive failures++ 
        |
        +-- below failure_threshold: still Healthy
        |
        +-- at threshold: alive=false, log "backend unhealthy"
                |
                v
        Pool.Next() skips it; traffic continues on the rest
```

## Recovery lifecycle

```
Unhealthy backend, health probe succeeds
        |
        v
Backend.RecordSuccess: consecutive successes++
        |
        +-- below recovery_threshold: still Unhealthy
        |
        +-- at threshold: alive=true, log "backend recovered"
                |
                v
        Pool.Next() includes it again
```

Recovery is driven only by active probes. Proxied traffic never reaches an unhealthy backend, so it cannot produce success signals.

## Concurrency model (summary)

Pool membership is immutable. Health flags are atomics written under a per-backend mutex. The round-robin cursor is one atomic counter. Details in [concurrency.md](concurrency.md).
