#!/usr/bin/env python3
"""Run every Grafana panel query against the running Prometheus.

A dashboard whose panels return nothing is worse than no dashboard: it reads as
"the system is idle" rather than "this query is wrong", and nobody notices
until the one incident it was built for.

Every panel here is expected to return a value on a healthy stack that has seen
some traffic, with one exception: a handful of queries describe things that
have not happened, and there is no honest way to make them return a number.
Those are listed in EXPECTED_EMPTY with the reason, so "returned nothing" stays
a finding rather than becoming background noise.

Where a metric can be defaulted honestly the panel query says `or vector(0)` —
but only on an ungrouped query. vector(0) carries no labels, so against a
`sum by (...)` it never matches the left side and is simply appended as a
permanent phantom series labelled "Value".

    task up
    task load          # give it something to measure
    task dashboards:check
"""
import json
import pathlib
import sys
import urllib.error
import urllib.parse
import urllib.request

PROM = "http://localhost:%s/api/v1/query" % (sys.argv[1] if len(sys.argv) > 1 else "9092")
DASHBOARDS = pathlib.Path(__file__).resolve().parents[2] / "deploy/grafana/dashboards"

# Queries with no series on a stack where nothing has gone wrong. Each one is
# grouped, so `or vector(0)` cannot default it without inventing a phantom
# series, and each describes an event a healthy run does not produce.
EXPECTED_EMPTY = {
    "sum by (result) (rate(job_leases_expired_total[15m]))":
        "no worker has died or wedged",
    "sum by (result) (rate(config_reloads_total[15m]))":
        "no SIGHUP has been sent",
    "sum by (state) (job_queue_depth)":
        "the queue is drained",
    "job_queue_oldest_pending_age_seconds":
        "the queue is drained",
}


def query(expr):
    url = PROM + "?" + urllib.parse.urlencode({"query": expr})
    try:
        with urllib.request.urlopen(url, timeout=10) as response:
            return json.load(response)
    except urllib.error.URLError as err:
        sys.exit("cannot reach Prometheus at %s: %s\nis the stack up? (task up)" % (PROM, err))


def main():
    checked, bad, skipped = 0, [], []
    for path in sorted(DASHBOARDS.glob("*.json")):
        dashboard = json.loads(path.read_text())
        for panel in dashboard["panels"]:
            for t in panel.get("targets", []):
                checked += 1
                body = query(t["expr"])
                if body["status"] != "success":
                    bad.append((path.name, panel["title"], t["expr"], body.get("error", "error")))
                elif not body["data"]["result"] and t["expr"] not in EXPECTED_EMPTY:
                    bad.append((path.name, panel["title"], t["expr"], "returned no series"))
                elif not body["data"]["result"]:
                    skipped.append((panel["title"], EXPECTED_EMPTY[t["expr"]]))

    if bad:
        print("%d of %d panel queries returned nothing:\n" % (len(bad), checked))
        for name, title, expr, why in bad:
            print("  %s — %s: %s" % (name, title, why))
            print("      %s\n" % expr.replace("\n", " "))
        return 1

    print("all %d panel queries returned data" % (checked - len(skipped)))
    for title, why in skipped:
        print("  (%s is empty: %s)" % (title, why))
    return 0


if __name__ == "__main__":
    sys.exit(main())
