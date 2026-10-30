#!/usr/bin/env bash
# Starts the demo server (5-10 ms latency, freezes for 300 ms every 3 s) and
# measures it twice: closed model (10 virtual users) and open model (1000 rps).
# Same server, same stalls: compare the p99 lines to see coordinated omission.
set -euo pipefail
cd "$(dirname "$0")/.."

ADDR="${ADDR:-127.0.0.1:8099}"
DURATION="${DURATION:-15s}"
mkdir -p bin out
go build -o bin/ ./cmd/...

./bin/demoserver --addr "$ADDR" --latency 5ms --jitter 5ms --stall-every 3s --stall-for 300ms 2>/dev/null &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -fs "http://$ADDR/" >/dev/null 2>&1 && break; sleep 0.1; done

echo "== closed model: 10 virtual users, $DURATION"
./bin/loadgen --quiet --vus 10 --duration "$DURATION" --out-md out/closed.md "http://$ADDR/"

echo
echo "== open model: 1000 requests/second, $DURATION"
set +e
./bin/loadgen --quiet --rate 1000 --duration "$DURATION" \
  --fail-if 'p99>250ms' --fail-if 'error_rate>1%' \
  --out-json out/open.json --out-md out/open.md "http://$ADDR/"
code=$?
set -e
echo
echo "open-model exit code: $code (2 = a --fail-if threshold was breached)"
echo "reports: out/closed.md out/open.md out/open.json"
