#!/usr/bin/env bash
# Break the webhook receiver and watch the circuit open, then heal it.
#
# The pairing is the point, and neither half is enough alone. The breaker turns
# a slow failure into a fast one. On its own that would be worse for the jobs:
# they would burn their whole retry budget in milliseconds of instant refusals
# and be discarded for an outage that had nothing to do with them. The snooze is
# what stops that — a job parked by an open circuit keeps its attempt.
set -euo pipefail

COMPOSE="docker compose -f deploy/compose.yaml"
API="localhost:${API_PORT:-8080}"
SINK="localhost:${SINK_PORT:-8081}"

psql() { $COMPOSE exec -T postgres psql -U taskapi -d taskapi -tAc "$1"; }
states() { psql "SELECT state || '=' || count(*) FROM jobs GROUP BY state" | tr '\n' ' '; }
budget() { psql "SELECT 'max attempt used: ' || coalesce(max(attempt), 0) || ' of ' || coalesce(max(max_attempts), 0) FROM jobs"; }

echo "==> the sink starts failing every delivery with 503"
curl -sf -X POST "$SINK/control" -d '{"down":true,"reset":true}' >/dev/null

echo "==> creating twelve tasks, which is twelve deliveries into a dead endpoint"
for n in $(seq 1 12); do
  curl -sf -o /dev/null -X POST "$API/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"during the outage $n\",\"status\":0}"
done

echo "==> waiting for the circuit to open"
for _ in $(seq 1 60); do
  $COMPOSE logs worker 2>&1 | grep -q 'circuit breaker opened' && break
  sleep 1
done
$COMPOSE logs worker 2>&1 | grep -o '{[^{]*circuit breaker opened[^}]*}' | tail -1 | sed 's/^/    /'

echo "==> what the jobs are doing while it is open"
echo "    $(states)"
echo "    $(budget | sed 's/^ *//')"
echo "    scheduled = snoozed. The attempt was given back, so a dependency"
echo "    outage is not spending the jobs' retry budget."

echo "==> the sink comes back"
curl -sf -X POST "$SINK/control" -d '{"down":false}' >/dev/null

for _ in $(seq 1 120); do
  [ "$(psql "SELECT count(*) FROM jobs WHERE state <> 'succeeded'" | tr -d ' ')" = 0 ] && break
  sleep 1
done
$COMPOSE logs worker 2>&1 | grep -o '{[^{]*circuit breaker closed[^}]*}' | tail -1 | sed 's/^/    /'
echo "    $(states)"

echo "==> at the sink"
curl -sf "$SINK/stats" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("    deliveries=%d  distinct events=%d" % (d["received"], d["distinct"]))
print("    every change survived the outage.")
'
