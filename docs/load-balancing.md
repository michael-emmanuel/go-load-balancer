# Load Balancing From First Principles

## What a load balancer does

A load balancer presents one address to clients and spreads their requests over several backends. It buys three things: capacity (more machines than one can serve), availability (a dead backend does not take the service down), and operability (backends can be added, removed or upgraded without clients noticing).

## Layer 4 vs Layer 7

A Layer 4 balancer works on TCP/UDP connections. It picks a backend when a connection opens and forwards bytes without understanding them. It is fast and protocol-agnostic, but it cannot route by URL, retry a single failed request, or inspect headers.

A Layer 7 balancer terminates the client's HTTP request, understands it, and issues a new request to the backend. It can route by path or header, return its own errors (503), add headers, and treat each request independently.

This project is Layer 7. It uses `httputil.ReverseProxy`, which parses the client request and creates a separate outbound request. A consequence: one client connection can fan out over many backends request by request.

## Reverse proxy fundamentals

A forward proxy acts for the client (the client knows about the proxy and asks it to reach arbitrary servers). A reverse proxy acts for the servers (the client thinks it is talking to the origin). A load balancer is a reverse proxy with a selection policy.

What the proxy does to each request: parse it, remove hop-by-hop headers (`Connection`, `Keep-Alive`, `Transfer-Encoding`, and anything named in `Connection`), rewrite the target URL, add `X-Forwarded-For/Host/Proto`, send it on a pooled backend connection, and stream the response back. See the test `TestStripsHopByHopHeaders`.

## Round-robin

Backends A, B, C receive requests in the order A, B, C, A, B, C. It needs no per-backend measurement, is trivially fair for uniform requests, and has no state beyond a counter.

### How the index advances

`Pool.Next` takes a ticket `t` from an atomic counter (0, 1, 2, ...), counts the healthy backends `h`, and returns the `(t mod h)`-th healthy backend.

With all healthy: tickets 0..5 map to A B C A B C.

With B unhealthy: `h = 2`, so tickets map onto {A, C} as A C A C.

### Why not "skip to the next healthy one"?

The common implementation is `idx = counter % n`, then walk forward until a healthy backend is found. With B down, ticket 1 lands on B and walks to C, and ticket 2 lands on C. C receives twice the traffic of A (A C C A C C). Indexing into the healthy set instead gives even distribution. The cost is an O(n) pass to count, which is negligible for tens of backends and allocation-free.

### What happens when membership changes

The ticket counter is global, so when a backend is ejected or recovers the position in the rotation shifts (for example a request consumed by a failed attempt still used a ticket). Over any window with a stable healthy set, distribution is exact. During changes it is merely close to even, which is acceptable: round-robin promises fairness over time, not a fixed global order across failures.

### Concurrency

Two concurrent requests get distinct tickets from `atomic.Add`, so they cannot pick the same slot by accident of interleaving. The test `TestConcurrentSelectionIsEvenlyDistributed` asserts exactly 1000 selections per backend from 3000 concurrent calls.

### Where round-robin falls short

It assumes equal backends and equal requests. A backend that is slower, smaller, or stuck on a few expensive requests still gets the same share.

## Other algorithms (not implemented)

Weighted round-robin: each backend has a weight; a backend with weight 3 gets three turns per cycle. Use when capacities differ. Smooth weighted round-robin (NGINX) interleaves turns instead of bursting them.

Least connections: send to the backend with the fewest in-flight requests. Adapts to slow backends and uneven request cost. Needs an in-flight counter per backend (atomic inc/dec around the proxied call) and a min-search, and it is vulnerable to a freshly recovered backend with zero connections being flooded unless combined with slow start.

Random / power of two choices: pick two backends at random and use the less loaded. Needs no shared cursor, which helps with multiple balancer instances, and gets most of the benefit of least-connections.

Consistent hashing: hash a key (client IP, session id, URL) onto a ring of backends so the same key maps to the same backend, and only a small fraction of keys move when backends change. Used for caches and sharded state.

Sticky sessions: pin a client to a backend, by cookie or by hashing. Useful when backends keep per-session local state, at the cost of uneven load and trouble when the pinned backend dies. Prefer externalizing session state.
