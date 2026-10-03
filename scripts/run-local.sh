#!/usr/bin/env bash
# Starts three demo backends (8081-8083) and the load balancer (8080).
# A 1s health interval is used so failure and recovery are quick to observe.
# Press Ctrl-C to stop everything.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
go build -o bin/backend ./cmd/backend
go build -o bin/load-balancer ./cmd/load-balancer

pids=()
cleanup() {
  trap - INT TERM EXIT
  echo "stopping..."
  kill "${pids[@]}" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup INT TERM EXIT

for port in 8081 8082 8083; do
  ./bin/backend --port "$port" >"bin/backend-$port.log" 2>&1 &
  pids+=($!)
  echo "backend $port pid=$!"
done

LB_HEALTH_INTERVAL=1s LB_HEALTH_TIMEOUT=500ms ./bin/load-balancer &
pids+=($!)
echo "load balancer pid=$! on :8080"

cat <<MSG

Try:
  curl http://localhost:8080/            # repeat to see round-robin
  curl http://localhost:8080/health      # load balancer health
  curl http://localhost:8080/metrics     # counters and per-backend state
  kill <backend pid>                     # then curl again; traffic skips it
  ./bin/backend --port 8082 &            # restart it; it rejoins after ~2s
MSG
wait
