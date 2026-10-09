# Go Load Balancer

A small HTTP load balancer built from first principles in Go, using only the standard library. It implements round-robin selection, active health checks with failure ejection and recovery, reverse proxying, graceful shutdown, and basic observability, and it is organized so the design reasoning is as inspectable as the code.

This is an educational systems project. It deliberately implements a simplified subset of what production load balancers and reverse proxies (NGINX, Envoy, HAProxy, cloud load balancers) provide. It is not intended for production deployment.

## Overview

The load balancer accepts HTTP requests, picks a healthy backend in round-robin order, and forwards the request using `net/http/httputil.ReverseProxy`. A background goroutine probes each backend's `/health` endpoint. Backends that fail are removed from rotation (not deleted) and rejoin automatically when they recover.

Standard library only. No third-party dependencies. `go.sum` is empty by design.

## What This Project Teaches

- Round-robin scheduling with a lock-free atomic cursor, and why the cursor advances over the healthy set only
- Reverse proxy fundamentals: request rewriting, hop-by-hop headers, X-Forwarded-\*, error mapping
- Active health checking, passive failure signals, and hysteresis thresholds against flapping
- Health state as a state machine (Healthy, Unhealthy, Healthy) rather than add/delete
- Concurrency design: which state is shared, who writes it, and the minimum synchronization that protects it
- Context propagation: client cancellation, probe timeouts, shutdown
- Graceful shutdown ordering and goroutine lifecycle
- Deterministic testing of concurrent and failure-prone code with `httptest` and the race detector

## Architecture

```
                         Client
                            |
                            v
        +-----------------------------------------+
        |              Load Balancer              |
        |                                         |
        |   server    HTTP listener, lifecycle    |
        |     |                                   |
        |     v                                   |
        |   proxy     ReverseProxy per backend    |
        |     |                                   |
        |     v                                   |
        |   balancer  Pool.Next() round-robin     |
        |     ^                                   |
        |     |  RecordSuccess / RecordFailure    |
        |   HealthChecker (background goroutine)  |
        +-----------------------------------------+
              /              |              \
             v               v               v
         Backend A       Backend B       Backend C
```

Responsibilities are split four ways: backend identity and health state (`balancer/backend.go`), selection policy (`balancer/balancer.go`), request forwarding (`proxy`), and lifecycle wiring (`server`). Details in [docs/architecture.md](docs/architecture.md).

## Request Flow

```
Client
  |
  v
Load Balancer  --- Pool.Next() ---> chooses next healthy backend
  |                                 (none healthy -> 503)
  +----> Backend A
  |
  +----> Backend B
  |
  +----> Backend C
```

1. The request reaches the `server` mux. `/health` and `/metrics` are served locally; everything else goes to the proxy.
2. `Pool.Next()` takes a ticket from an atomic counter and picks the matching healthy backend.
3. The per-backend `ReverseProxy` rewrites the request (target URL, `X-Forwarded-*`), strips hop-by-hop headers, and forwards it over a pooled connection.
4. The backend response is streamed back unchanged. Transport errors become 502 (connection/protocol failure) or 504 (timeout).

## Health Check Flow

```
every health_check_interval:
    for each backend, concurrently:
        GET <backend>/health   (timeout = health_check_timeout)
        2xx            -> RecordSuccess
        error/timeout/non-2xx -> RecordFailure

Healthy --(failure_threshold consecutive failures)--> Unhealthy
Unhealthy --(recovery_threshold consecutive successes)--> Healthy
```

Backends start Unhealthy. A synchronous check runs before the server begins serving, so no request is routed on unverified state.

## Failure Handling

| Situation                              | Client sees                       | Load balancer does                            |
| -------------------------------------- | --------------------------------- | --------------------------------------------- |
| Backend down, not yet detected         | 502 for requests routed to it     | Counts a passive failure; ejects at threshold |
| Backend down, detected                 | Nothing unusual                   | Skips it; traffic continues on healthy nodes  |
| Backend slow to answer                 | 504 after response-header timeout | Counts a passive failure                      |
| Backend slow on /health                | Nothing unusual                   | Probe times out; backend ejected              |
| Malformed response / closed connection | 502                               | Counts a passive failure                      |
| All backends unhealthy                 | 503                               | Logs a warning; keeps probing for recovery    |
| Client disconnects                     | (gone)                            | Logs; does not penalize the backend           |
| SIGINT/SIGTERM                         | In-flight requests complete       | Drains, then exits                            |

Full analysis in [docs/failure-handling.md](docs/failure-handling.md).

## Project Structure

```
cmd/load-balancer/      entry point: flags, env, signals
cmd/backend/            demo upstream that prints "backend=<port>"
internal/balancer/      Backend, Pool (round-robin), HealthChecker
internal/proxy/         ReverseProxy wiring, error mapping, counters
internal/config/        defaults, YAML-subset file, env overrides
internal/server/        mux, startup check, graceful shutdown
internal/testutil/      controllable fake backends for tests
docs/                   design documentation and engineer review guide
examples/config.yaml    sample configuration
scripts/run-local.sh    local demo launcher
```

## Getting Started

Requires Go 1.22 or newer.

```
git clone <this repository> && cd load-balancer
make build
```

The module path is `github.com/michael-emmanuel/load-balancer`

## Running the Demo

In four terminals (or run `./scripts/run-local.sh` to start everything at once):

```
go run ./cmd/backend --port 8081
go run ./cmd/backend --port 8082
go run ./cmd/backend --port 8083
go run ./cmd/load-balancer
```

Then:

```
curl http://localhost:8080/
curl http://localhost:8080/
curl http://localhost:8080/
```

To observe failure and recovery, stop the backend on 8082 (Ctrl-C), wait a few health-check intervals, and request again: traffic flows to 8081 and 8083 only. Restart the backend and it rejoins automatically. Use `LB_HEALTH_INTERVAL=1s LB_HEALTH_TIMEOUT=500ms` to make this fast. Inspect state with `curl localhost:8080/metrics`.

## Testing

```
go test ./...
```

## Race Detection

```
go test -race ./...
```

The suite includes tests designed for the race detector: concurrent selection, health flipping while requests are in flight, and concurrent proxied requests. See [docs/testing.md](docs/testing.md).

## Example Behavior

Captured from a real run (health interval 1s):

```
$ for i in 1 2 3 4 5 6; do curl -s localhost:8080/; done
backend=8081
backend=8082
backend=8083
backend=8081
backend=8082
backend=8083

# kill the backend on 8082, wait for detection
$ for i in 1 2 3 4; do curl -s localhost:8080/; done
backend=8081
backend=8083
backend=8081
backend=8083

$ curl -s localhost:8080/health
{"status":"ok","healthy_backends":2,"total_backends":3}

# restart 8082, wait for recovery
$ for i in 1 2 3 4 5 6; do curl -s localhost:8080/; done
backend=8082
backend=8083
backend=8081
backend=8082
backend=8083
backend=8081

# kill everything
$ curl -s -o /dev/null -w "status=%{http_code}\n" localhost:8080/
status=503
```

Corresponding log lines:

```
level=WARN msg="backend unhealthy" backend=http://localhost:8082 error="... connection refused"
level=INFO msg="backend recovered" backend=http://localhost:8082
level=WARN msg="no healthy backend available" method=GET path=/
level=INFO msg="shutting down" timeout=15s
level=INFO msg="shutdown complete"
```

## Design Decisions

- Atomic ticket counter plus a count-and-walk over the healthy set, instead of a mutex or a rebuilt slice. See [docs/load-balancing.md](docs/load-balancing.md) and [docs/concurrency.md](docs/concurrency.md).
- Health is a state transition, not removal from a list. See [docs/health-checks.md](docs/health-checks.md).
- Selection, health state, and forwarding are separate concerns in separate packages.
- Backends start unhealthy (fail closed) and a startup check runs before serving.
- No retries by default, because they are unsafe for non-idempotent requests.

Every decision is documented in the Problem / Decision / Reasoning / Trade-off / Production Evolution format in [docs/design-tradeoffs.md](docs/design-tradeoffs.md).

## Trade-offs

- O(n) selection per request, chosen over a rebuilt healthy list for zero allocation and simplicity.
- Fixed-interval active checks: simple and predictable, but detection latency is up to interval times failure_threshold.
- In-memory state: nothing to persist or coordinate, but a restart forgets all health history.
- Single process: no shared state to coordinate, but one failure domain.

## Limitations

- Layer 7 HTTP/1.1 reverse proxy only. No TLS termination, no HTTP/2 to backends, no WebSocket-specific handling.
- Static backend list from configuration. No service discovery or hot reload.
- Plain round-robin ignores backend capacity and in-flight load.
- No retries, circuit breaking, rate limiting, or authentication.
- Single instance; the load balancer itself is a single point of failure.
- Metrics are a JSON endpoint, not a Prometheus exposition.

## Production Extensions

Not implemented; discussed in [docs/design-tradeoffs.md](docs/design-tradeoffs.md) and the engineering review guide: weighted and least-connections algorithms, retries with budgets, circuit breakers and outlier detection, connection draining, service discovery, TLS termination, distributed tracing, and horizontal scaling of the load balancer tier.

## Engineer Review

Round-robin under concurrency, atomic vs mutex, health-check design and flapping, why retries are dangerous, graceful shutdown ordering, and scaling to many load balancers and backends. Start with [docs/engineering-review.md](docs/engineering-review.md).

## Engineering Lessons

- State that is read on every request and written rarely deserves a lock-free read path.
- A cursor over a changing set needs care: advancing over the healthy set, not the full set, keeps distribution even.
- Cancellation must be distinguished from failure: a client hanging up and a shutdown in progress are not backend faults.
- Timing-based tests need generous failure bounds and event-based synchronization. During development of this project a 20ms probe timeout made a test flaky under the race detector, which is the same failure mode as a real false positive.
