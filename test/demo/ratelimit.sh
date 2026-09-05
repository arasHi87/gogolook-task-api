#!/usr/bin/env bash
# Show the two limiter tiers, and what a refused caller is told.
#
# A rate limiter protects you from your callers; a circuit breaker protects you
# from your dependencies. They point in opposite directions. This is the
# inbound one, and its quota is per tier — which is the reason it is worth
# having at all, rather than one global number that either starves the paying
# client or hands the same capacity to anyone who can open a socket.
set -euo pipefail

API="localhost:${API_PORT:-8080}"
TOKEN="${DEMO_TOKEN:-demo-standard-token}"

# One line per response: status, and the quota headers that came back.
burst() {
  local label=$1 n=$2
  shift 2
  echo "==> $label: $n requests as fast as curl will send them"
  for _ in $(seq 1 "$n"); do
    curl -sS -o /dev/null -D - "$@" "$API/api/v1/tasks" \
      | awk '
          NR==1        {status=$2}
          /^[Rr]ate[Ll]imit:/            {sub(/^[^:]*: */,""); sub(/\r$/,""); rl=$0}
          /^[Rr]etry-[Aa]fter:/          {sub(/^[^:]*: */,""); sub(/\r$/,""); ra=$0}
          END          {printf "    %s  %s%s\n", status, rl, (ra=="" ? "" : "  retry-after=" ra)}'
  done
}

echo "==> the policy each tier is served under"
curl -sS -o /dev/null -D - "$API/api/v1/tasks" \
  | awk '/^[Rr]ate[Ll]imit-[Pp]olicy:/ {sub(/\r$/,""); print "    anonymous  " $0}'
curl -sS -o /dev/null -D - -H "Authorization: Bearer $TOKEN" "$API/api/v1/tasks" \
  | awk '/^[Rr]ate[Ll]imit-[Pp]olicy:/ {sub(/\r$/,""); print "    standard   " $0}'
echo

burst "anonymous, keyed by IP" 25
echo
burst "the same burst with a token" 25 -H "Authorization: Bearer $TOKEN"

cat <<'NOTE'

    The anonymous caller hits the wall; the token holder does not. The quota
    followed the token rather than the address, which is the point of having an
    identity at all here — it is a quota dimension, not a gate.

    Honest caveat: this limiter is per replica. Three replicas behind a load
    balancer enforce three times the nominal limit. Real deployments enforce it
    at the edge with a shared counter; this is the backstop for when the edge is
    misconfigured.
NOTE
