# Health Checks

## Active health checks (implemented)

The load balancer itself sends `GET <backend>/health` every `health_check_interval` and treats a 2xx response within `health_check_timeout` as healthy. Anything else (connection refused, timeout, non-2xx, redirect) is a failure.

Implementation notes:

- One goroutine runs the loop; each round probes backends concurrently and waits for all probes, so a round takes at most one timeout, not n timeouts.
- Each probe uses `context.WithTimeout` derived from the loop's context, so shutdown cancels in-flight probes.
- Probes use a dedicated client with keep-alives disabled. Each probe opens a fresh TCP connection, so a backend whose accept path is broken cannot appear healthy through a stale pooled connection.
- Redirects are not followed.
- If the parent context is cancelled, the probe result is discarded. Shutting down must not mark every backend unhealthy.

## Passive health checks (partly implemented)

Passive checking watches real traffic. When a proxied request fails at the transport level (connection refused, reset, malformed response, timeout) the proxy calls `Pool.ReportFailure`, which feeds the same failure counter as active checks. This ejects a dead backend after `failure_threshold` failed requests without waiting for the next probe.

Not implemented: ejecting on a high rate of application 5xx responses (outlier detection). A backend returning 500 is answering; the proxy passes it through and does not count it as a transport failure.

Recovery is active-only. An ejected backend gets no traffic, so only probes can show it has recovered.

## Liveness vs readiness

Liveness asks "is the process alive?" and a failed liveness check typically means restart. Readiness asks "can it take traffic now?" and a failed readiness check means stop routing, but do not restart. A load balancer cares about readiness: a backend warming a cache or draining should fail `/health` without being killed. The demo backend's `/health` is trivially ready; a real one should check dependencies it cannot serve without, while avoiding checks so deep that a shared dependency outage ejects every backend at once.

The load balancer's own `/health` is a readiness signal for the layer in front of it: 200 while at least one backend is healthy, 503 otherwise.

## Timeouts

The probe timeout must be shorter than the interval (enforced by config validation). It trades off false positives (too short: a GC pause or load spike looks like a failure) against detection latency (too long). During development a 20ms timeout made a test flaky under the race detector because the very first probe sometimes exceeded it, which is exactly a false positive.

## Failure detection and recovery

Worst-case detection time for a hard failure is roughly `interval * failure_threshold` (a probe can fail right after a successful one). With defaults (5s, threshold 2) that is up to 10s, shortened by passive failures if traffic is flowing. Recovery time is about `interval * recovery_threshold`.

Startup: backends begin Unhealthy and a synchronous round runs before the server accepts traffic. The cost is slightly slower startup; the benefit is never routing to an unverified backend.

## Flapping

A backend that alternates between passing and failing causes constant churn. The thresholds are the defense: it must fail `failure_threshold` times in a row to leave and succeed `recovery_threshold` times in a row to return. The defaults are symmetric (2 and 2). Production systems often readmit more cautiously than they eject, which is a one-line configuration change here. A failure resets the success streak, covered by `TestThresholdsProvideHysteresis`.

## False positives and false negatives

False positive: a healthy backend is ejected (probe timed out due to a transient pause, or a network blip between the balancer and one backend). Cost: reduced capacity, possible overload of the rest. Mitigation: thresholds, sensible timeouts, a cap on how much of the pool may be ejected at once (not implemented).

False negative: a broken backend stays in rotation (`/health` returns 200 while real endpoints fail, or the failure falls between probes). Cost: user-visible errors. Mitigation: meaningful readiness checks, passive checks, outlier detection.

## Health-check load

Cost is n probes per interval, each a TCP handshake plus a tiny request. At 3 backends and 5s that is negligible. At thousands of backends and many balancer instances, probe traffic itself becomes load; production systems share health state, probe from a subset of nodes, or use jittered intervals so probes do not synchronize.

## How production systems improve on this

- Passive outlier detection on error rate and latency
- Slow start (ramp traffic after recovery) and ejection time that grows with repeated ejections
- Panic threshold: if too many backends are unhealthy, route to all of them rather than overloading the few remaining
- Jittered intervals, richer checks (gRPC health protocol, TCP-only, custom expected body)
- Shared or aggregated health state across balancer instances
