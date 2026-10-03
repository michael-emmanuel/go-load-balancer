# Design Trade-offs

Each entry: Problem, Decision, Reasoning, Trade-off, Production Evolution.

## 1. Mutex vs atomic counter

Problem: Every request must advance a shared cursor, and requests are concurrent.
Decision: `atomic.Uint64` ticket counter; per-backend mutex only for multi-field health transitions.
Reasoning: Incrementing one integer is exactly what an atomic add does, with no lock handoff and no risk of holding a lock across slow work. A mutex over the cursor would serialize requests for no benefit. Multi-field state still needs a mutex because separate atomics cannot be updated together.
Trade-off: Atomics are easy to misuse beyond a single variable. The count-then-walk selection sees health state that can change mid-way (benign, documented).
Production Evolution: At very high rates, shard the counter per CPU or avoid a shared cursor with random or power-of-two-choices selection.

## 2. In-memory backend state

Problem: Health state needs a home.
Decision: Plain structs in process memory.
Reasoning: State is cheap to rebuild (one probe round) and a stale persisted value would be worse than none.
Trade-off: A restart forgets history (ejection counts, last error). Multiple balancer instances each learn health independently.
Production Evolution: Share or aggregate health through a control plane or service discovery system.

## 3. Active health checks

Problem: Dead backends must be detected even with no traffic, and recovery must be detected at all.
Decision: Periodic probes of `/health`, plus a passive failure signal from the proxy.
Reasoning: Only active probes can discover recovery, because an ejected backend receives no traffic.
Trade-off: Probe load grows with backends times balancer instances; detection is not instantaneous; `/health` can lie.
Production Evolution: Outlier detection on real traffic, jittered intervals, richer checks, shared probing.

## 4. Fixed health-check interval

Problem: How often to probe.
Decision: One configurable interval, with a timeout that must not exceed it.
Reasoning: Predictable load and detection time that can be stated in one line (`interval * failure_threshold`).
Trade-off: Fast detection costs more probes; slow detection costs more errors. No backoff for repeatedly failing backends.
Production Evolution: Per-backend adaptive intervals, exponential ejection time, jitter, faster probing of recently failed nodes.

## 5. Simple round-robin

Problem: Choose a backend.
Decision: Round-robin over the healthy set.
Reasoning: No measurements, easy to reason about and test, fair for homogeneous backends and requests.
Trade-off: Ignores backend capacity and current load; a slow backend gets full share.
Production Evolution: Weighted, least-request, power of two choices, locality-aware, with slow start.

## 6. ReverseProxy from the standard library

Problem: Forwarding HTTP correctly (hop-by-hop headers, streaming, trailers, upgrades, flushing, body handling) is full of edge cases.
Decision: Use `httputil.ReverseProxy` with `Rewrite`, one instance per backend, sharing one `Transport`.
Reasoning: It is battle-tested and the clearest example of the reverse proxy model. `Rewrite` operates on a copy of the request and is safer than the older `Director`. Hand-rolling forwarding would add bugs, not insight.
Trade-off: Less control; the error handling hooks and transport settings are the only seams. A per-backend instance means a map lookup per request.
Production Evolution: A proxy with filters, retries, HTTP/2, per-route policy (Envoy, NGINX).

## 7. No persistent state

Problem: Should configuration or health survive restarts?
Decision: No. Configuration comes from file/env at startup; health is rediscovered by the startup probe round.
Reasoning: Nothing here is expensive to recompute and there is nothing to corrupt.
Trade-off: No hot reload; changing backends requires a restart.
Production Evolution: Dynamic configuration from a control plane or service discovery, with atomic swap of an immutable pool snapshot.

## 8. Single load-balancer process

Problem: Availability and capacity of the balancer itself.
Decision: One process.
Reasoning: Keeps the lesson on selection, health and forwarding without coordination.
Trade-off: Single point of failure and a throughput ceiling.
Production Evolution: Multiple instances behind DNS, anycast or a cloud L4 balancer, with health state per instance.

## 9. No distributed coordination

Problem: With several balancers, health and round-robin position would otherwise need agreement.
Decision: None.
Reasoning: Each instance deciding independently is correct enough: round-robin fairness does not require a global order, and each instance probes for itself.
Trade-off: Instances can disagree briefly about health; combined distribution is only statistically even.
Production Evolution: Eventually consistent shared health, or deliberately independent decisions (the common choice).

## 10. No TLS termination

Problem: Production traffic needs TLS.
Decision: Plain HTTP listener and plain HTTP to backends.
Reasoning: TLS adds certificates and configuration that obscure the load-balancing lessons.
Trade-off: Not deployable on untrusted networks.
Production Evolution: `ListenAndServeTLS` with reloadable certificates, SNI, optional mTLS to backends, usually terminated at an edge tier.

## 11. No connection draining beyond graceful shutdown

Problem: Removing a backend (or the balancer) should not drop requests.
Decision: On shutdown, `http.Server.Shutdown` waits for in-flight requests. A backend ejected by health checks simply receives no new requests; requests already in flight to it continue.
Reasoning: Covers the common case with standard library behavior.
Trade-off: No drain mode where `/health` first reports not-ready while the listener stays open; no per-backend drain for planned removals.
Production Evolution: A drain state per backend, and a pre-stop phase where the balancer fails its own readiness check before closing listeners.

## 12. No retries by default

Problem: A failed request could be retried on another backend.
Decision: No retries.
Reasoning: Retrying a non-idempotent request (POST) can execute it twice. Retries also multiply load during an outage; a backend at 100% failure with 3 attempts per request takes 3x the traffic, and nested retries across tiers multiply (retry storms). The request body may also already be consumed.
Trade-off: A request that hits a dying backend returns an error instead of succeeding elsewhere.
Production Evolution: Retry only idempotent methods or requests that failed before any bytes were sent, with a per-request attempt cap, a retry budget (for example no more than 10% extra load), backoff with jitter, and a different backend on each attempt.

## 13. Backends start unhealthy

Problem: Before the first probe, health is unknown.
Decision: Fail closed, and run a synchronous probe round before serving.
Reasoning: Sending traffic to unverified backends produces avoidable errors on every restart.
Trade-off: A slightly delayed start, and total unavailability if the first round fails everywhere.
Production Evolution: Optimistic start for balancers that restart often, or inherit health from peers.
