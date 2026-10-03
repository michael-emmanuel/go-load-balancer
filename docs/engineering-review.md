# Load Balancer Engineering Review

This guide separates what is implemented in this repository from what is a proposed extension. Where an answer describes something not built here, it says so.

## 1. Intro

This is an HTTP load balancer in Go from scratch using only the standard library. It keeps a pool of backends, routes requests round-robin to healthy ones through a reverse proxy, and runs background health checks that eject failing backends and readmit them when they recover. Focused on concurrency correctness, failure handling, and graceful shutdown, and tested with fake backends and the race detector.

## 2. Brief Explanation

Problem: spread HTTP traffic over several servers without clients knowing, and keep working when one dies.

Architecture: four small pieces. A `Backend` holds a URL and health state. A `Pool` holds the backends and the round-robin cursor. A `proxy.Handler` forwards requests using `httputil.ReverseProxy`. A `HealthChecker` goroutine probes `/health` and updates backend state. A `Server` wires them together and owns startup and shutdown.

Round-robin: an atomic counter hands out tickets; a ticket modulo the number of healthy backends picks the backend. Indexing into the healthy set (rather than skipping dead nodes) keeps distribution even when one is down.

Health checks: every interval, probe each backend concurrently with a timeout. Consecutive failures past a threshold mark a backend unhealthy; consecutive successes bring it back. The backend is never deleted, so recovery is just a state change. Proxy transport errors also count as failures, so a crash is noticed before the next probe.

Request forwarding: `ReverseProxy` rewrites the target, strips hop-by-hop headers, sets `X-Forwarded-For`, and streams the response. Connection errors map to 502, timeouts to 504, no healthy backend to 503.

Concurrency: the pool is immutable; the cursor is one atomic; health is an atomic flag mirrored from mutex-protected state; no lock is held during a request. `go test -race` passes, with tests built to exercise concurrent selection and health flips.

Failure and shutdown: client cancellation is distinguished from backend failure. On SIGTERM the health loop is cancelled, the listener closes, in-flight requests drain up to a timeout, and the process waits for its goroutines before exiting.

## 3. Architecture Walkthrough

1. Client sends a request to the load balancer's single address.
2. The `server` mux serves `/health` and `/metrics` itself and sends everything else to the proxy.
3. The backend pool is a fixed list of `Backend` values, built from configuration.
4. Backend selection: `Pool.Next()` returns a healthy backend or nil.
5. Reverse proxy: that backend's `ReverseProxy` forwards the request and returns the response.
6. Health checker: independent goroutine, probes backends, updates their state.
7. Failure state: backends failing past a threshold are Unhealthy and invisible to `Next`.
8. Recovery: successful probes past a threshold flip them back; traffic resumes automatically.

## 4. Core Data Structures

`Backend` (balancer/backend.go)

- Responsibility: identity (`URL`) and health state of one upstream.
- Invariants: `alive` changes only inside a threshold-crossing transition under `mu`; counters reset appropriately (failures reset on success; successes reset on failure); `URL` is immutable.
- Concurrency: `alive` is an `atomic.Bool` so the request path reads it lock-free; everything that must change together lives under `mu`. Transitions return a bool so exactly one caller logs and reacts.

`Pool` (balancer/balancer.go)

- Responsibility: ordered backends and the round-robin cursor; `Next()`.
- Invariants: membership never changes after construction; the cursor only increases.
- Concurrency: the slice needs no lock because it is never mutated; the cursor is an `atomic.Uint64`.

`HealthChecker` (balancer/healthcheck.go)

- Responsibility: probe backends and report results.
- Invariants: rounds never overlap; every probe is bounded by the timeout; a cancelled parent context records nothing.
- Concurrency: owns one loop goroutine plus per-round probe goroutines joined by a `WaitGroup`.

`proxy.Handler` (proxy/proxy.go)

- Responsibility: forward requests, map errors to statuses, count totals.
- Invariants: one `ReverseProxy` per backend, built once; a single shared `Transport`.
- Concurrency: the map is read-only after construction; counters are atomics.

`Server` (server/server.go)

- Responsibility: lifecycle. Startup check, serve, graceful shutdown, join background goroutines.

## 5. Round-Robin

Backends A, B, C, all healthy. Tickets 0, 1, 2, 3 come from an atomic counter; the healthy count is 3:

```
Request 1 (ticket 0): 0 mod 3 = 0 -> A
Request 2 (ticket 1): 1 mod 3 = 1 -> B
Request 3 (ticket 2): 2 mod 3 = 2 -> C
Request 4 (ticket 3): 3 mod 3 = 0 -> A
```

B becomes unhealthy. The healthy set is now [A, C], count 2:

```
ticket 4: 4 mod 2 = 0 -> A
ticket 5: 5 mod 2 = 1 -> C
ticket 6: 6 mod 2 = 0 -> A
```

Why index the healthy set: the alternative, `ticket mod 3` then "walk to the next healthy", sends ticket 1 to B-then-C and ticket 2 to C, so C gets twice A's share. The cost of my approach is an O(n) count per request.

What a changing set does: tickets are global, so when the set changes the position shifts (a ticket consumed by a failed request, for example). Fairness holds across stable periods, not as a fixed global order. One of my tests initially had the wrong expectation for exactly this reason.

## 6. Health Check

Why: without them, a dead backend keeps receiving its share of traffic until each client request fails, and a recovered backend never gets traffic back.

How: a goroutine ticks every interval; each round sends `GET /health` to every backend concurrently with `context.WithTimeout`. A 2xx is success; an error, timeout or any other status is failure.

Detection: a backend is marked Unhealthy after `failure_threshold` consecutive failures. Proxied-request transport errors count too, so a dead node can be ejected faster than the probe interval.

Recovery: an Unhealthy backend gets no traffic, so only probes can see it return. After `recovery_threshold` consecutive successes it is Healthy again.

Subtleties worth volunteering: probes disable keep-alive so each tests a fresh connection; cancelled-context results are discarded so shutdown does not eject everyone; the threshold gives hysteresis against flapping.

## 7. Reverse Proxy

Why `httputil.ReverseProxy`: forwarding HTTP correctly means handling hop-by-hop headers, streaming bodies and flushing, trailers, and connection reuse. The standard implementation does that and is widely used. The interesting engineering is in selection, health and failure handling, so that is where this project spends effort.

`client -> backend`: the client opens a connection to the server and the server answers. `client -> load balancer -> backend`: two independent HTTP exchanges. The balancer terminates the client's request, then makes its own request to the backend over a pooled connection, then copies the response back. Consequences: the backend sees the balancer's address (hence `X-Forwarded-For`), connections to backends are reused across clients, the balancer can answer on its own (503), and a failure on either leg is separately observable.

Details: `Rewrite` (not `Director`) sets the target URL and forwarded headers on a copy of the request; one `Transport` is shared so connection pooling works; proxy settings from the environment are ignored for internal backends.

## 8. Concurrency

"Is this thread-safe?" Yes. All shared state is either immutable after construction, atomic, or under a mutex, and `go test -race` passes on tests that stress the shared paths.

"What shared state exists?" The round-robin counter, each backend's alive flag and failure bookkeeping, and request counters. The pool membership and proxy map are immutable.

"Why did you use a mutex?" For the backend's multi-field transition: failure count, alive flag, last error and timestamp must change together, and two goroutines racing to flip state must not both claim the transition. Atomics cannot update several fields as one step.

"Could you use atomics?" For the cursor and counters, yes, and I did. For the health state I could pack it into one atomic word or compare-and-swap an immutable state struct pointer, which would make even the transition lock-free. I chose the mutex because transitions are rare, the code is easier to verify, and the hot path already reads an atomic.

"What happens if health checks and requests modify backend state simultaneously?" The health goroutine and the proxy error path both call `RecordFailure`; they serialize on the backend's mutex. Request-path reads of the alive flag are lock-free atomics, so a request sees either the old or new state, never a torn one. A request may be routed to a backend that was ejected a moment earlier; that request just fails and is handled like any other failure.

## 9. Failure Scenarios

"What happens if a backend dies?" Requests routed to it before detection get 502 and each counts as a passive failure. After the threshold, or the next failed probe rounds, it is Unhealthy and skipped. Remaining backends carry the load.

"What happens if all backends die?" `Next` returns nil and the balancer returns 503 immediately. Its own `/health` also returns 503. Probing continues, and the first recovery restores service.

"What happens if a backend is slow?" Slow `/health` times out and the backend is ejected. A slow application response hits the response-header timeout, returns 504, and counts as a failure. What I do not have is load-aware routing or per-request deadlines propagated to backends, so one slow backend that still passes health checks keeps receiving its share.

"What happens if a backend recovers?" Probes succeed `recovery_threshold` times in a row, it flips to Healthy, and the next tickets include it. There is no slow start; a production balancer would ramp traffic.

"What happens during shutdown?" Health context is cancelled, the listener closes, in-flight requests get up to `shutdown_timeout` to finish, leftovers are force-closed, and the process waits for the health goroutine before exiting.

## 10. Complexity

Algorithmic:

- Backend selection: O(n) time, O(1) space (two passes over n backends, no allocation). n is small, so this is a handful of atomic loads.
- Health check round: O(n) probes started; they run concurrently, so wall time is bounded by one timeout, not n.
- Request forwarding: O(size of request and response) for streaming copy, O(1) for the routing decision.
- Backend state update: O(1) under a per-backend mutex.

Network I/O dominates: a proxied request costs at least a round trip to the backend, typically milliseconds, versus nanoseconds for selection. Optimizing selection from O(n) to O(1) would not be measurable unless there were thousands of backends. The relevant I/O costs are connection setup (amortized by pooling), bytes copied, and backend latency. Health checks cost n network round trips per interval, a load on the backends and the network that is independent of request volume.

## 11. Bottlenecks

- Single process: a throughput ceiling and a single point of failure.
- Lock contention: only the one atomic cursor is globally shared on the hot path (cache-line contention at very high rates); per-backend counters contend per backend.
- Health-check overhead: n probes per interval per balancer instance; matters at large n or many instances.
- Network bandwidth: the balancer carries every request and response byte twice (in and out); NIC bandwidth is often the real limit for large responses.
- Connection limits: file descriptors and ephemeral ports; two sockets per proxied request; `MaxIdleConnsPerHost` bounds idle reuse.
- Backend capacity: the balancer can only spread load, not create capacity, and round-robin ignores uneven capacity.

## 12. Scaling the Design

From 1 balancer and 3 backends to many of each:

- Multiple balancer instances: run N identical stateless instances. Each keeps its own health view and cursor; fairness is statistical, which is fine.
- Distributing traffic across instances: DNS round-robin (cheap, slow to change, cached by clients); anycast (same IP announced from many places, routing picks the nearest); a cloud L4 balancer or ECMP in front, which hashes connections to instances.
- Hundreds or thousands of backends: O(n) selection and n probes per interval start to matter. Switch to power-of-two-choices (O(1)), probe with jitter, and shard probing or share results.
- Service discovery: replace the static list with a registry (DNS SRV, Consul, Kubernetes endpoints). Make the pool an immutable snapshot swapped atomically so readers stay lock-free.
- Distributed health state: either independent per instance (simple, resilient) or aggregated through a control plane (consistent, more complex).
- Consistent hashing: when state or cache locality matters, such as sharded caches, so membership changes remap few keys.
- Connection pooling: already per backend; tune idle limits and consider HTTP/2 multiplexing to backends.
- TLS termination: at the balancer or an edge tier, with certificate rotation; optional mTLS to backends.
- Observability: Prometheus metrics, access logs with request IDs, tracing headers.
- Autoscaling: scale backends on load metrics; the balancer must tolerate fast membership changes (slow start, drain).

## 13. Production Improvements

Implemented in this repository: round-robin, active and partial passive health checks, thresholds, 502/503/504 mapping, graceful shutdown, structured logs, a JSON metrics endpoint, and a race-tested concurrency model.

Proposed, not implemented:

- Configuration management: hot reload, validation of changes, atomic pool swap.
- Service discovery.
- Retries: only for idempotent or not-yet-sent requests, with attempt caps, retry budgets and jittered backoff.
- Circuit breakers: stop sending to a backend that is erroring, probe cautiously (half-open).
- Connection draining: per-backend drain state and a pre-shutdown readiness failure.
- Outlier detection: eject on error rate or latency, with exponentially growing ejection time and a cap on ejected percentage.
- Weighted routing and least-request.
- TLS termination and mTLS.
- Authentication and request limits.
- Metrics (Prometheus), tracing, access logs.
- Rate limiting and load shedding.
- Distributed deployment and capacity management, including a panic threshold.

## 14. Q&As

1. Why round-robin? It needs no measurements, is simple to reason about, and is fair when backends and requests are similar. It is wrong when they are not, which is when you move to weighted or least-request.

2. What happens when a node fails? Requests routed to it before detection fail with 502 and count as failures; at the threshold it is marked Unhealthy and skipped; it stays in the pool and returns when probes succeed.

3. How do health checks work? A goroutine probes `/health` on every backend each interval, concurrently, with a timeout. Consecutive-result thresholds drive Healthy to Unhealthy and back.

4. How do you prevent race conditions? Immutable pool, an atomic cursor, atomics for hot-path reads, a per-backend mutex for multi-field transitions, no lock held during network I/O, and the race detector over concurrent tests.

5. Why not use atomic operations everywhere? Atomics make single-word operations indivisible. The health transition updates several fields that must be consistent with each other, and individual atomics would let a reader see a half-updated combination. A mutex states that invariant directly.

6. What happens when all nodes fail? `Next` returns nil and the balancer returns 503 without contacting any backend; it keeps probing and recovers automatically.

7. How would you scale this? See section 12: stateless balancer instances behind DNS, anycast or an L4 balancer; service discovery; O(1) selection; shared or independent health state.

8. What if one backend is much slower? Health checks may not catch it, and round-robin keeps sending it a full share. Fix with least-request or power of two choices, latency-based outlier detection, and per-request timeouts.

9. How would you implement weighted round-robin? Give each backend a weight. Simple form: expand the rotation so a weight-3 backend appears three times. Better: smooth weighted round-robin, where each backend accumulates its weight each turn, the highest current value is chosen, and the total weight is subtracted from it, which interleaves picks instead of bursting them. The selection step needs a small lock or a snapshot because it updates several values.

10. How would you implement least-connections? Keep an atomic in-flight counter per backend, incremented before proxying and decremented after (use `defer`). Selection picks the healthy backend with the smallest count; with many backends, sample two and take the smaller (power of two choices). Add slow start so a newly recovered backend with zero connections is not flooded.

11. How would you prevent a flapping node from repeatedly entering and leaving service? Hysteresis (separate failure and recovery thresholds, already implemented), ejection time that grows on each repeat ejection, and a cap on how much of the pool can be ejected.

12. How would you implement connection draining? Add a Draining state: no new requests, existing ones finish. Track in-flight per backend, remove after it reaches zero or a timeout. For the balancer itself, first fail the readiness endpoint so upstream balancers stop sending, wait, then close the listener.

13. How would you add retries? Retry only when safe: idempotent methods, or failures before any request bytes were written (connection refused). Buffer or re-create the body, pick a different backend, cap attempts at 2 or 3, and enforce a total deadline.

14. Why can retries be dangerous? Non-idempotent requests may execute twice. Retries multiply load on an already struggling system, and when several tiers each retry, load multiplies (3 tiers with 3 attempts each can mean 27 times). That turns a partial failure into a full outage.

15. How would you avoid retry storms? Retry budgets (retries limited to a fraction of total traffic), exponential backoff with jitter, retry only at one layer, circuit breakers, and propagated deadlines so work is abandoned once the caller has given up.

16. How would you implement sticky sessions? Hash a stable key (client IP, cookie, session ID) onto the healthy set, ideally with consistent hashing so changes remap few clients; or set an affinity cookie naming the backend. Fall back to normal selection when the pinned backend is unhealthy. Better still, move session state out of the backends.

17. How would you support TLS? Use `ListenAndServeTLS` or a `tls.Config` with certificate reloading (`GetCertificate`), supporting SNI; optionally re-encrypt to backends (`https://` backends already work with the same proxy and a configured root pool); automate renewal. At scale, terminate at an edge tier.

18. How would you make this highly available? Run multiple instances behind a floating IP (keepalived/VRRP), DNS, anycast or a cloud L4 balancer; keep instances stateless; health-check the balancers themselves (the `/health` endpoint here returns 503 when no backend is healthy); deploy across failure domains.

19. How would you test backend failures? The way this repo does: real `httptest` servers I can close, make hang, or flip unhealthy, plus raw TCP servers that hang up or send garbage; synchronous health rounds for determinism; the race detector; `-count=N` repeated runs to surface flakes. Beyond unit tests: fault injection in staging (kill processes, add latency with a proxy such as Toxiproxy, partition networks).

20. What are the main bottlenecks? Single-process throughput, NIC bandwidth, file descriptors and connection counts, backend capacity, and at scale health-check traffic and the contended cursor.

21. Why is the health state a state machine and not add and remove? Removing loses the backend's configuration and identity, and recovery then needs something else to re-add it. Keeping it and flipping a flag makes recovery the same code path as failure and keeps the pool immutable and lock-free to read.

22. Why start backends unhealthy? Fail closed: do not send real traffic to a backend you have not verified. A synchronous startup probe keeps the cost small.

23. Why distinguish client cancellation from backend failure? A client giving up says nothing about backend health; counting it would let impatient clients eject healthy backends.

24. Why does the proxy not log query strings? They often carry tokens and personal data. Logging method, path and backend is enough to understand routing.

## 15. Design Q&As

"Design a load balancer capable of handling 1 million requests per second."
Reasoning path: one machine handles perhaps tens to a few hundred thousand proxied HTTP requests per second, bounded by CPU and NIC, so the answer is a tier. Put an L4 layer (ECMP or anycast, connection hashing) in front of many stateless L7 instances. Keep L7 instances share-nothing so they scale linearly. Remove per-request contention (per-CPU counters or random selection, no shared locks). Reuse backend connections and use HTTP/2 to backends. Watch bandwidth: 1M requests at 10 KB responses is 80 Gbit/s. Autoscale on CPU and connection counts.

"How would you distribute traffic across multiple load balancer instances?"
Reasoning path: clients need one stable name. Options by layer: DNS returning multiple addresses (simple, slow failover); anycast (routing-based, fast failover, needs network control); an L4 balancer using consistent hashing so a connection stays on one instance; client-side balancing. Choose based on failover speed and who controls the network. Instances need no shared state.

"How would you handle 100,000 backend servers?"
Reasoning path: O(n) selection and n probes per interval stop being negligible. Shard the fleet into clusters or subsets (each balancer instance handles a subset, as in deterministic subsetting). Use power-of-two-choices for O(1). Probe with jitter, from few vantage points, or rely mainly on passive signals. Distribute the pool via service discovery with incremental updates, not full lists.

"How would you detect unhealthy servers faster?"
Reasoning path: detection time is roughly interval times threshold. Reduce the interval (more load), add passive detection from real traffic (free and immediate; implemented in part here), probe faster after a first failure, and use latency and error-rate outlier detection. The limit is false positives, so pair speed with confirmation.

"How would you avoid false positives?"
Reasoning path: false positives come from single transient failures. Require consecutive failures, use timeouts above normal latency spikes, require agreement from multiple balancers or probes, cap the percentage of ejected backends (panic threshold), and make readiness checks shallow enough that a shared dependency does not eject everyone.

"How would you prevent one unhealthy backend from repeatedly receiving traffic?"
Reasoning path: it is the flapping problem plus bad ejection logic. Exponential ejection time per repeat, success thresholds on recovery, slow start after readmission so a fragile backend is not hit with full load, and circuit breaking with a half-open trial request.

"How would you handle long-lived WebSocket connections?"
Reasoning path: the connection is pinned to one backend for hours, so round-robin only decides at connect time and load can become uneven. `ReverseProxy` supports Upgrade by hijacking and copying bytes both ways, but graceful shutdown does not track hijacked connections, so you need your own registry to close them with a notice. Prefer least-connections for placement, add idle and ping timeouts, and plan for reconnect storms when a node dies (jittered client backoff).

"How would you support HTTP/2?"
Reasoning path: HTTP/2 multiplexes many requests on one client connection, which the L7 balancer already handles request by request. Go's `http.Server` negotiates h2 automatically over TLS. For backends enable h2 on the Transport (`ForceAttemptHTTP2`) so a few connections carry many requests; then per-connection load balancing at L4 would be uneven, which is a reason to balance per request at L7. Watch head-of-line blocking at the TCP level and stream limits.

"How would you support TLS termination?"
Reasoning path: terminate at the balancer so it can read HTTP (needed for L7 routing); get certificates (ACME or secret store) with hot reload; choose TLS versions and ciphers; decide whether to re-encrypt to backends (usually yes, with mTLS inside the trust boundary); pass client identity via headers; consider session resumption and CPU cost, and offload to a dedicated edge tier at scale.

"How would you perform zero-downtime backend deployments?"
Reasoning path: take the backend out of rotation first, then stop it, then bring it back. Mark it draining (or fail its readiness endpoint) so the balancer stops sending new requests; wait for in-flight to finish; stop; deploy; start; wait until readiness passes the recovery threshold; ramp traffic with slow start. Roll through backends a few at a time so capacity never drops below demand, and abort on elevated errors.

## 16. What I Would Change Next

Correctness

1. Add a per-backend draining state and a pre-shutdown readiness failure.
2. Propagate request deadlines to backends and bound total request time.
3. Add a panic threshold so a global probe failure does not eject everything.

Performance 4. Add least-request or power-of-two-choices selection behind a small `Selector` interface. 5. Benchmark `Next` and the proxy path; shard the cursor only if profiling shows contention.

Reliability 6. Safe retries (idempotent or pre-send failures) with a budget and backoff. 7. Outlier detection with growing ejection time, plus slow start after recovery. 8. Jitter in the probe interval.

Observability 9. Prometheus-format metrics, request IDs, and structured access logs with latency. 10. Per-backend latency histograms and ejection counters.

Scalability 11. Dynamic backend set from service discovery with an atomically swapped immutable pool. 12. Multi-instance deployment guidance with health aggregation and a TLS termination tier.
