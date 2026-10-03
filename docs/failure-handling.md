# Failure Handling

For each failure: what happens, what the load balancer does, what the client sees, and how the system recovers.

## Connection refused

What happens: the backend process is down or not listening. `ReverseProxy` gets a dial error.
Load balancer: the error handler returns 502, calls `Pool.ReportFailure` (passive signal), and logs at error level. The next active probe also fails. At `failure_threshold` the backend is marked Unhealthy.
Client sees: 502 Bad Gateway for requests that were routed to it before ejection.
Recovery: when the backend restarts, `recovery_threshold` consecutive successful probes readmit it. Tests: `TestConnectionRefusedReturns502AndEjectsBackend`, `TestConnectionRefusedAndRestart`.

## Timeout

What happens: the backend accepts the connection but does not send response headers within `ResponseHeaderTimeout` (default 30s).
Load balancer: returns 504, counts a passive failure. The timeout is a transport setting, not a request deadline, so a backend streaming a long body is not cut off.
Client sees: 504 Gateway Timeout.
Recovery: the backend is probed as usual; if `/health` is also slow it is ejected, and readmitted when probes pass. Test: `TestSlowBackendResponseReturns504`.

## Backend crash (mid-request or silent close)

What happens: the connection closes without a response, or with a malformed one.
Load balancer: the transport returns an error; the proxy returns 502 and records a passive failure. If headers were already sent to the client, `ReverseProxy` aborts the response and the client sees a truncated body or a reset, which is the only honest signal possible at that point.
Client sees: 502, or a broken response if the failure happened mid-stream.
Recovery: as for connection refused. Tests: `TestBackendClosesConnectionReturns502`, `TestMalformedBackendResponseReturns502`.

## Unhealthy backend (health endpoint fails but process is up)

What happens: `/health` returns non-2xx or times out.
Load balancer: records failures; after the threshold, marks the backend Unhealthy and stops routing to it. The backend object stays in the pool.
Client sees: nothing; requests go elsewhere.
Recovery: once `/health` passes `recovery_threshold` times in a row, the backend rejoins. Tests: `TestFailingBackendIsMarkedUnhealthyAndRecovers`, `TestSlowBackendIsDetectedAndExcluded`.

## All backends unavailable

What happens: `Pool.Next` returns nil.
Load balancer: returns 503 Service Unavailable immediately (no attempt to contact a known-bad backend), logs a warning, and keeps probing. `/health` on the load balancer also returns 503 so an upstream balancer can drain it.
Client sees: 503 with a short text body.
Recovery: the first backend to pass its recovery threshold restores service. Tests: `TestNoHealthyBackendsReturns503`, `TestHealthAndRoutingReturn503WhenAllBackendsUnhealthy`.

Not implemented: a panic threshold (routing to unhealthy backends anyway when too few remain healthy).

## Health-check failure (the checker itself)

What happens: the probe cannot be built or the network between balancer and backend is partitioned.
Load balancer: treated as a failure of that backend; each probe is bounded by the timeout so a hung probe cannot stall the round or leak goroutines. If the checker's own context is cancelled, the result is discarded instead of being recorded as a failure.
Risk: a balancer-side network problem looks like every backend failing. A production system would detect "all fail at once" and avoid ejecting everything.

## Request cancellation

What happens: the client disconnects or its deadline passes. The request context is cancelled and the outbound request to the backend is aborted.
Load balancer: logs at info level. It does not count a failure and does not touch the backend's health, because the backend did nothing wrong.
Client sees: nothing; it has gone. Test: `TestClientCancellationDoesNotEjectBackend`.

## Shutdown during active traffic

What happens: SIGINT or SIGTERM arrives while requests are in flight.
Load balancer: cancels the health context, closes the listener so no new connections are accepted, and waits up to `shutdown_timeout` for active requests to finish. If the deadline passes, remaining connections are force-closed and `Run` returns the deadline error. It then waits for the health goroutine to exit. A second signal terminates the process immediately.
Client sees: in-flight requests complete normally; new connection attempts are refused. Requests still running at the deadline are cut off.
Recovery: n/a, the process exits. Tests: `TestGracefulShutdownDrainsInFlightRequests`, `TestShutdownTimeoutForcesClose`.

Gap: HTTP keep-alive clients are told to close after their current request, but there is no separate connection-draining phase where `/health` goes unhealthy first so an upstream balancer can stop sending traffic before the listener closes. See design-tradeoffs.md.

## Scenario walkthroughs

Scenario 1, three healthy backends: A, B, C, A, B, C (`TestDistributesRoundRobin`).

Scenario 2, B fails: the next probe round ejects B; traffic becomes A, C, A, C (`TestFailedBackendIsSkippedThenRejoins`).

Scenario 3, B recovers: probes succeed, B is readmitted, traffic includes B again.

Scenario 4, all fail: 503 until a backend recovers.

Scenario 5, slow backend: its probe exceeds the timeout, counts as a failure, and it is ejected without ever serving a client request (`TestHealthCheckTimeoutMarksBackendUnhealthy`, `TestSlowBackendIsDetectedAndExcluded`).
