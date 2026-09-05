#!/usr/bin/env python3
"""Run every Grafana panel query against the running Prometheus.

A dashboard whose panels return nothing is worse than no dashboard: it reads as
"the system is idle" rather than "this query is wrong", and nobody notices
until the one incident it was built for.

Every panel here is expected to return a value on a healthy stack that has seen
some traffic. Where a metric legitimately has no series when nothing has gone
wrong — no throttling, no dead-lettered jobs, no reclaimed leases — the panel
query says `or vector(0)` so it draws a flat zero instead of an absence.

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


def query(expr):
    url = PROM + "?" + urllib.parse.urlencode({"query": expr})
    try:
        with urllib.request.urlopen(url, timeout=10) as response:
            return json.load(response)
    except urllib.error.URLError as err:
        sys.exit("cannot reach Prometheus at %s: %s\nis the stack up? (task up)" % (PROM, err))


def main():
    checked, bad = 0, []
    for path in sorted(DASHBOARDS.glob("*.json")):
        dashboard = json.loads(path.read_text())
        for panel in dashboard["panels"]:
            for t in panel.get("targets", []):
                checked += 1
                body = query(t["expr"])
                if body["status"] != "success":
                    bad.append((path.name, panel["title"], t["expr"], body.get("error", "error")))
                elif not body["data"]["result"]:
                    bad.append((path.name, panel["title"], t["expr"], "returned no series"))

    if bad:
        print("%d of %d panel queries returned nothing:\n" % (len(bad), checked))
        for name, title, expr, why in bad:
            print("  %s — %s: %s" % (name, title, why))
            print("      %s\n" % expr.replace("\n", " "))
        return 1

    print("all %d panel queries returned data" % checked)
    return 0


if __name__ == "__main__":
    sys.exit(main())
