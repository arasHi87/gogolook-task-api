#!/usr/bin/env bash
# Kill the worker mid-job and watch the lease reaper hand its work to another.
#
# What this shows, and the part worth watching: the job completes exactly once,
# but the webhook is delivered twice. The killed worker's HTTP call reached the
# sink and it died before it could record success, so the reaper handed the job
# back and it was delivered again. That is at-least-once, working correctly —
# and it is why the handler sends X-Event-Id and the README says the receiver
# must deduplicate.
set -euo pipefail

COMPOSE="docker compose -f deploy/compose.yaml"
API="localhost:${API_PORT:-8080}"
SINK="localhost:${SINK_PORT:-8081}"

psql() { $COMPOSE exec -T postgres psql -U taskapi -d taskapi -tAc "$1"; }
states() { psql "SELECT state || '=' || count(*) FROM jobs GROUP BY state" | tr '\n' ' '; }

echo "==> slowing the sink so jobs are still in flight when the worker dies"
curl -sf -X POST "$SINK/control" -d '{"latency":"6s","reset":true}' >/dev/null

echo "==> creating four tasks"
for n in 1 2 3 4; do
  curl -sf -o /dev/null -X POST "$API/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"in flight $n\",\"status\":0}"
done
sleep 2
echo "    $(states)"

echo "==> SIGKILL the worker (a crash, not a drain: no chance to release anything)"
docker kill --signal=KILL taskapi-worker-1 >/dev/null
sleep 1
echo "    $(states)   <- still 'running', held by a worker that no longer exists"

echo "==> restarting; the reaper reclaims the expired leases"
curl -sf -X POST "$SINK/control" -d '{"latency":"0s"}' >/dev/null
$COMPOSE up -d worker >/dev/null 2>&1

for _ in $(seq 1 90); do
  [ "$(psql "SELECT count(*) FROM jobs WHERE state <> 'succeeded'" | tr -d ' ')" = 0 ] && break
  sleep 1
done
echo "    $(states)"

echo "==> what the reaper logged"
$COMPOSE logs worker 2>&1 | grep -o 'expired leases reclaimed[^}]*}' | tail -1 | sed 's/^/    /'

echo "==> the job's own record of it"
psql "SELECT 'attempt=' || attempt || ' workers=' || array_length(attempted_by,1) || ' first_error=' || (errors->0->>'error') FROM jobs WHERE attempt > 1 LIMIT 1" | sed 's/^/    /'

echo "==> at the sink"
curl -sf "$SINK/stats" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("    deliveries=%d  distinct events=%d  duplicates=%d" % (d["received"], d["distinct"], d["duplicates"]))
print("    the job ran twice, so the event was delivered twice; a receiver that")
print("    deduplicates on X-Event-Id sees %d, which is the contract." % d["distinct"])
'
