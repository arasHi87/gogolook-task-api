#!/usr/bin/env bash
# Send the same write twice and watch it happen once.
#
# The header is the producer half of at-least-once. The queue guarantees an
# event is delivered at least once; this guarantees a request that is *sent*
# twice — a timeout the client could not tell from a failure, a retrying proxy
# — is *executed* once.
set -euo pipefail

COMPOSE="docker compose -f deploy/compose.yaml"
API="localhost:${API_PORT:-8080}"
KEY="demo-$(date +%s)-$RANDOM"

psql() { $COMPOSE exec -T postgres psql -U taskapi -d taskapi -tAc "$1"; }
count() { psql "SELECT count(*) FROM $1" | tr -d ' '; }

send() {
  curl -sS -D /tmp/taskapi-demo-headers -o /tmp/taskapi-demo-body \
    -X POST "$API/tasks" \
    -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $KEY" \
    -d "$1"
  # To stderr, so a caller capturing this function's output gets the body and
  # only the body — the two are compared byte for byte below.
  printf '    status=%s replayed=%s\n' \
    "$(awk 'NR==1{print $2}' /tmp/taskapi-demo-headers | tr -d '\r')" \
    "$(grep -i '^idempotency-replayed:' /tmp/taskapi-demo-headers | awk '{print $2}' | tr -d '\r' || echo '-')" >&2
  cat /tmp/taskapi-demo-body
}

before_tasks=$(count tasks)
before_jobs=$(count jobs)

echo "==> first attempt, with Idempotency-Key: $KEY"
first=$(send '{"name":"pay invoice 4471","status":0}')
echo "    $first"

echo "==> the client never saw that response, so it retries the identical request"
second=$(send '{"name":"pay invoice 4471","status":0}')
echo "    $second"

echo "==> the two bodies"
if [ "$first" = "$second" ]; then
  echo "    byte-identical — the second was replayed from the store, not re-executed"
else
  echo "    DIFFER, which is a bug:"
  diff <(echo "$first") <(echo "$second") || true
fi

echo "==> what the database actually did"
printf '    tasks +%s   jobs +%s   (one write, one event)\n' \
  "$(( $(count tasks) - before_tasks ))" "$(( $(count jobs) - before_jobs ))"

echo "==> the same key with a different request is the client's bug, not a retry"
curl -sS -o /dev/null -w '    status=%{http_code}  (422 = key reuse)\n' \
  -X POST "$API/tasks" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" \
  -d '{"name":"pay invoice 9999","status":0}'

echo "==> the stored key"
psql "SELECT 'state=' || state || ' status=' || status_code || ' bytes=' || length(response_body)
        FROM idempotency_keys WHERE key = '$KEY'" | sed 's/^/    /'

rm -f /tmp/taskapi-demo-headers /tmp/taskapi-demo-body
