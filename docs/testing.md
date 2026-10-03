# Testing Strategy

Run `go test ./...` and `go test -race ./...`. All tests use only the standard library.

## Unit tests

Package `balancer` (`balancer_test.go`): round-robin order, exclusion of unhealthy backends, recovery, no-healthy case, threshold hysteresis, URL validation. These use fake URLs and never touch the network. They prove the algorithm and state machine in isolation.

Package `config`: parsing, precedence, validation errors, and that `examples/config.yaml` actually loads.

## Integration tests with httptest.Server

`internal/testutil` provides `Backend`, an `httptest.Server` with controllable behavior: healthy or not, slow health, a blockable `/slow` endpoint, status passthrough, and an echo endpoint. `Stop` and `Start` simulate a crash and restart on the same address.

Package `proxy` builds a real pool, health checker and handler, serves the handler with `httptest.NewServer`, and sends real HTTP requests. These prove forwarding of method, path, query, headers and body; hop-by-hop stripping; status and header passthrough; 502 and 504 mapping; and the five failure scenarios end to end.

Package `server` runs the full process wiring on port 0.

## Failure injection

Failures are injected at the boundary where they would really occur: a closed listener (connection refused), a raw TCP server that hangs up or writes garbage (crash and malformed response), a handler that blocks (timeout), and a health endpoint that returns 503 or hangs. No mocks of internal types are needed, which keeps tests honest about the real code path.

## Concurrency tests and the race detector

`TestConcurrentSelectionIsEvenlyDistributed` issues 3000 concurrent selections and asserts exactly 1000 per backend: this fails if the cursor is not atomic. `TestSelectionWhileHealthFlips` flips a backend's health in a tight loop while many goroutines select. `TestConcurrentRequests` sends 500 concurrent requests through the proxy and checks totals and per-backend counts. `-race` turns any unsynchronized access in these paths into a failure.

## Deterministic testing

Techniques used to avoid sleeps:

- Health rounds are driven synchronously with `CheckAll`; no ticker is involved.
- Handler tests call `ServeHTTP` directly when asynchrony is not the point (cancellation test).
- Event channels replace sleeps: `Backend.Entered` signals that a request is in flight; gates release blocked handlers.
- Where the real ticker must be exercised (`Run` tests), `testutil.Eventually` polls with a five-second failure bound. The bound is not an expected wait; tests normally finish in milliseconds.
- The only intentionally short real timeouts are the 50ms health and response timeouts in the slow-backend tests, which assert that a timeout occurs. They cannot flake toward false passes; a slow machine only makes the timeout fire sooner relative to work, which is the expected outcome.

A lesson from this suite: a server test originally used a 20ms probe timeout. Under `-race` with repeated runs the startup probe occasionally exceeded it, the backend started unhealthy, and the first request got a 503. The code was right and the test configuration was wrong, and it is the same mechanism as a production false positive. It was found by running `-count=30`, which is worth doing for any concurrent test.

## Shutdown testing

`TestGracefulShutdownDrainsInFlightRequests` starts a request that blocks in the backend, waits for the backend to signal it arrived, cancels the server context, then asserts that: the listener closes, `Run` has not returned while the request is in flight, the request completes with 200 after release, and `Run` returns nil. `TestShutdownTimeoutForcesClose` asserts that a stuck request cannot block shutdown beyond `shutdown_timeout`. `TestRunStopsWhenContextIsCancelled` asserts the health loop exits, and `Server.Run` joins it before returning, which is the leak guarantee.

## What is not covered

No load or soak tests, no fuzzing of the config parser, no tests against real network faults (packet loss, partitions), and no HTTP/2 or WebSocket tests.
