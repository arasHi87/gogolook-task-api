#!/usr/bin/env python3
"""Generate the Grafana dashboards.

    task dashboards:generate

Grafana dashboards are JSON, and hand-edited JSON is where panels quietly stop
matching each other: one has a legend at the bottom and the next on the right,
one stacks and its twin does not, and a query gets fixed in one place and not
in the mirror of it two panels down. The panel helpers below are the reason
every panel here has the same shape.

Two conventions worth knowing before editing:

  * `or vector(0)` only belongs on an ungrouped query. vector(0) carries no
    labels, so against a `sum by (...)` it never matches and is simply appended
    — a permanent phantom series labelled "Value". On an ungrouped query the
    two do match, so the fallback appears only when there is genuinely nothing.
  * A ratio gets an explicit max of 1. A flat zero series with a percentunit
    axis and no ceiling auto-scales to 10000%, which is not wrong so much as
    useless.
"""
import json, pathlib

DS = {"type": "prometheus", "uid": "prometheus"}
OUT = pathlib.Path("deploy/grafana/dashboards")

def target(expr, legend, instant=False):
    return {"datasource": DS, "expr": expr, "legendFormat": legend,
            "refId": chr(65), "instant": instant, "range": not instant}

def _targets(exprs):
    ts = []
    for i, (expr, legend) in enumerate(exprs):
        t = target(expr, legend)
        t["refId"] = chr(65 + i)
        ts.append(t)
    return ts

def ts(title, exprs, unit="short", desc="", w=12, h=8, x=0, y=0, stack=False, minv=None, maxv=None):
    return {
        "type": "timeseries", "title": title, "description": desc,
        "datasource": DS, "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": _targets(exprs),
        "fieldConfig": {
            "defaults": {
                "unit": unit,
                "min": minv,
                "max": maxv,
                "custom": {
                    "drawStyle": "line", "lineWidth": 1, "fillOpacity": 12 if not stack else 40,
                    "showPoints": "never", "spanNulls": True,
                    "stacking": {"mode": "normal" if stack else "none", "group": "A"},
                },
                "color": {"mode": "palette-classic"},
            },
            "overrides": [],
        },
        "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                    "tooltip": {"mode": "multi", "sort": "desc"}},
    }

def stat(title, expr, unit="short", desc="", w=6, h=5, x=0, y=0, thresholds=None, legend=""):
    steps = [{"color": "green", "value": None}]
    for colour, value in (thresholds or []):
        steps.append({"color": colour, "value": value})
    return {
        "type": "stat", "title": title, "description": desc,
        "datasource": DS, "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": [target(expr, legend, instant=True)],
        "fieldConfig": {"defaults": {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps},
                                     "color": {"mode": "thresholds"}}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                    "colorMode": "value", "graphMode": "area", "textMode": "auto"},
    }

def timeline(title, exprs, desc="", w=24, h=7, x=0, y=0):
    return {
        "type": "state-timeline", "title": title, "description": desc,
        "datasource": DS, "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": _targets(exprs),
        "fieldConfig": {
            "defaults": {
                "custom": {"lineWidth": 0, "fillOpacity": 80},
                # Thresholds do the colouring, and they also win the label.
                # Grafana's state-timeline renders the threshold band label
                # ("< 1", "2+") in preference to a value mapping, whichever
                # form the mapping takes — so the value text is turned off
                # below and the colour key lives in the panel description,
                # where it is at least accurate. The mappings are kept because
                # the tooltip does honour them.
                "color": {"mode": "thresholds"},
                "thresholds": {"mode": "absolute", "steps": [
                    {"color": "green", "value": None},
                    {"color": "orange", "value": 1},
                    {"color": "red", "value": 2},
                ]},
                # Exact value maps, with decimals pinned to 0 so the gauge's
                # float renders as "2" rather than "2.0" — a value map compares
                # the *formatted* value, and quietly matches nothing otherwise.
                "decimals": 0,
                "mappings": [{"type": "value", "options": {
                    "0": {"text": "closed", "index": 0},
                    "1": {"text": "half-open", "index": 1},
                    "2": {"text": "open", "index": 2},
                }}],
            },
            "overrides": [],
        },
        "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.9,
                    "legend": {"displayMode": "list", "placement": "bottom", "showLegend": False}},
    }

def row(title, y):
    return {"type": "row", "title": title, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
            "collapsed": False, "panels": []}

def dashboard(uid, title, description, panels, tags):
    return {
        "uid": uid, "title": title, "description": description, "tags": tags,
        "schemaVersion": 39, "version": 1, "editable": True,
        "time": {"from": "now-1h", "to": "now"},
        "refresh": "10s",
        "timezone": "browser",
        "panels": panels,
        "templating": {"list": []},
        "annotations": {"list": []},
    }

# ---------------------------------------------------------------- API — RED
api = dashboard(
    "taskapi-api", "Task API — RED",
    "Rate, errors and duration for the public listener. The three questions a "
    "request-driven service is judged on, in the order you ask them.",
    [
        row("Right now", 0),
        stat("Requests / sec", "sum(rate(http_server_requests_total[5m]))", "reqps",
             "Rate: how much work is arriving.", x=0, y=1),
        stat("Error ratio", "job:http_error_ratio:rate5m", "percentunit",
             "Errors: the SLO numerator. 0.1% is the 99.9% objective.",
             x=6, y=1, thresholds=[("orange", 0.001), ("red", 0.01)]),
        stat("p99 latency",
             "histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))",
             "s", "Duration: the tail, not the mean. The objective is 500ms.",
             x=12, y=1, thresholds=[("orange", 0.25), ("red", 0.5)]),
        stat("In flight", "sum(http_server_active_requests)", "short",
             "Concurrency. This is what the in-flight limiter caps, and what a "
             "rate limiter cannot see.", x=18, y=1),

        row("Rate", 6),
        ts("Requests by route", [("sum by (route) (rate(http_server_requests_total[5m]))", "{{route}}")],
           "reqps", "Templated routes only: the label is finite by construction.",
           x=0, y=7, stack=True, minv=0),
        ts("Requests by status", [("sum by (status) (rate(http_server_requests_total[5m]))", "{{status}}")],
           "reqps", "429 and 503 here are the guards working, not the service failing.",
           x=12, y=7, stack=True, minv=0),

        row("Errors", 15),
        ts("Error ratio", [("job:http_error_ratio:rate5m", "5xx ratio")], "percentunit",
           "The SLO. Burn-rate alerts fire off this same ratio over four windows.",
           x=0, y=16, minv=0, maxv=1),
        ts("Shed and throttled",
           [('(sum(rate(http_server_requests_total{status="429"}[5m])) or vector(0))', "429 rate limited"),
            ("(sum(rate(http_server_inflight_rejected_total[5m])) or vector(0))", "503 load shed")],
           "reqps", "Deliberate refusals. Sustained shedding is the service "
                    "protecting itself and still wants a human.",
           x=12, y=16, minv=0),

        row("Duration", 24),
        ts("Latency quantiles",
           [("histogram_quantile(0.50, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))", "p50"),
            ("histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))", "p95"),
            ("histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_seconds_bucket[5m])))", "p99")],
           "s", "The mean hides the tail, and the tail is what users notice.",
           x=0, y=25, minv=0),
        ts("p99 by route",
           [("histogram_quantile(0.99, sum by (le, route) (rate(http_server_request_duration_seconds_bucket[5m])))", "{{route}}")],
           "s", "Which endpoint is slow, rather than that something is.",
           x=12, y=25, minv=0),

        row("Callers", 33),
        ts("Requests by client",
           [("sum by (client_id) (rate(http_server_requests_total[5m]))", "{{client_id}}")],
           "reqps", "client_id is bounded by the configured client list plus the "
                    "literal 'anonymous'. Never an address.",
           x=0, y=34, stack=True, minv=0),
        ts("Body sizes",
           [("histogram_quantile(0.95, sum by (le) (rate(http_server_request_body_size_bytes_bucket[5m])))", "p95 request"),
            ("histogram_quantile(0.95, sum by (le) (rate(http_server_response_body_size_bytes_bucket[5m])))", "p95 response")],
           "bytes", "", x=12, y=34, minv=0),
    ],
    ["taskapi", "red"],
)

# -------------------------------------------------------------- Queue health
queue = dashboard(
    "taskapi-queue", "Task API — Queue health",
    "The queue's own vocabulary. The headline number is not throughput, it is "
    "the age of the oldest pending job: depth alone cannot tell ten thousand "
    "jobs draining in twenty seconds from five stuck for an hour.",
    [
        row("The health signal", 0),
        stat("Oldest pending job", "max(job_queue_oldest_pending_age_seconds or vector(0)) or vector(0)", "s",
             "THE signal. Alerts at 5 minutes. It catches a poison job blocking a "
             "partition, every worker wedged on a hung dependency, and a deploy "
             "that forgot to start the consumer.",
             w=8, h=6, x=0, y=1, thresholds=[("orange", 60), ("red", 300)]),
        stat("Backlog", "sum(job_queue_depth) or vector(0)", "short",
             "Ambiguous on its own, which is why it is beside the age and not "
             "instead of it.", w=8, h=6, x=8, y=1),
        stat("Worker utilisation",
             '(sum(job_queue_depth{state="running"}) or vector(0)) / clamp_min(sum(job_workers_configured), 1)',
             "percentunit", "Running jobs over configured workers, fleet-wide.",
             w=8, h=6, x=16, y=1, thresholds=[("orange", 0.8), ("red", 0.95)]),

        row("Backlog", 7),
        ts("Depth by state", [("sum by (state) (job_queue_depth)", "{{state}}")], "short",
           "available is claimable now; scheduled is snoozed or backing off; "
           "running is held by a worker; retryable is waiting for the scheduler.",
           x=0, y=8, stack=True, minv=0),
        ts("Oldest pending age by kind",
           [("job_queue_oldest_pending_age_seconds", "{{kind}}")], "s",
           "Queried on scrape, never written by a ticker: a gauge written by a "
           "goroutine keeps publishing a stale number with a fresh timestamp.",
           x=12, y=8, minv=0),

        row("Throughput", 16),
        ts("Outcomes", [("sum by (result) (rate(jobs_processed_total[5m]))", "{{result}}")],
           "ops", "succeeded, retried, cancelled, discarded, snoozed, lost. "
                  "snoozed is a dependency outage the job was not charged for.",
           x=0, y=17, stack=True, minv=0),
        ts("Enqueued", [("sum by (outcome) (rate(jobs_enqueued_total[5m]))", "{{outcome}}")],
           "ops", "deduped is the enqueue-time unique key working: the same "
                  "logical event offered twice while one is still live.",
           x=12, y=17, stack=True, minv=0),

        row("Latency", 25),
        ts("Wait: enqueue to claim",
           [("histogram_quantile(0.50, sum by (le) (rate(job_wait_duration_seconds_bucket[5m])))", "p50"),
            ("histogram_quantile(0.95, sum by (le) (rate(job_wait_duration_seconds_bucket[5m])))", "p95"),
            ("histogram_quantile(0.99, sum by (le) (rate(job_wait_duration_seconds_bucket[5m])))", "p99")],
           "s", "The latency a producer actually experiences. Measured by the "
                "database, so it is not two clocks subtracted.",
           x=0, y=26, minv=0),
        ts("Processing: claim to finalize",
           [("histogram_quantile(0.50, sum by (le) (rate(job_processing_duration_seconds_bucket[5m])))", "p50"),
            ("histogram_quantile(0.95, sum by (le) (rate(job_processing_duration_seconds_bucket[5m])))", "p95"),
            ("histogram_quantile(0.99, sum by (le) (rate(job_processing_duration_seconds_bucket[5m])))", "p99")],
           "s", "Wider buckets than HTTP on purpose: a job is allowed to be slow.",
           x=12, y=26, minv=0),

        row("Failure modes", 34),
        ts("Retry ratio",
           [("(sum(rate(jobs_retried_total[5m])) or vector(0)) / clamp_min(sum(rate(jobs_processed_total[5m])), 1e-9)", "retries / outcomes")],
           "percentunit", "Alerts above 25%. Work that is not progressing, "
                          "whatever the throughput graph says.",
           x=0, y=35, minv=0, maxv=1),
        ts("Leases reclaimed and jobs discarded",
           [("sum by (result) (rate(job_leases_expired_total[15m]))", "lease expired {{result}}"),
            ("(sum(rate(dead_letter_jobs_total[15m])) or vector(0))", "discarded")],
           "ops", "A reclaimed lease means a worker died or wedged holding work. "
                  "Self-healing, never routine.",
           x=12, y=35, minv=0),
        ts("Claim batch size",
           [("histogram_quantile(0.50, sum by (le) (rate(job_claim_batch_size_bucket[5m])))", "p50 claimed")],
           "short", "Consistently short of the configured batch is workers "
                    "contending on SKIP LOCKED, which looks like nothing else here.",
           x=0, y=43, minv=0),
        ts("Configured workers", [("sum(job_workers_configured)", "workers")], "short",
           "", x=12, y=43, minv=0),
    ],
    ["taskapi", "queue"],
)

# ------------------------------------------------------- Resilience & runtime
res = dashboard(
    "taskapi-resilience", "Task API — Resilience & runtime",
    "The guards in both directions, and the resources underneath them. A rate "
    "limiter protects you from your callers; a circuit breaker protects you "
    "from your dependencies.",
    [
        row("Outbound: the circuit", 0),
        timeline("Circuit state", [("max by (name) (circuit_breaker_state)", "{{name}}")],
                 "Green is closed, amber half-open, red open — hover for the "
                 "state name. Encoded as 0/1/2 by severity, so max-over-time "
                 "still means 'how bad did it get'.", y=1),
        ts("Transitions",
           [("sum by (name, to) (rate(circuit_breaker_transitions_total[5m]))", "{{name}} -> {{to}}")],
           "ops", "A circuit that opens and closes twelve times an hour looks "
                  "healthy in the gauge if the scrape lands between transitions.",
           x=0, y=8, minv=0),
        ts("Dependency latency",
           [("histogram_quantile(0.95, sum by (le, target) (rate(dependency_request_duration_seconds_bucket[5m])))", "p95 {{target}}"),
            ("histogram_quantile(0.99, sum by (le, target) (rate(dependency_request_duration_seconds_bucket[5m])))", "p99 {{target}}")],
           "s", "A breaker with no per-attempt timeout never trips: the calls "
                "hang rather than fail.",
           x=12, y=8, minv=0),

        row("Inbound: the limiter", 16),
        ts("Decisions by tier",
           [("sum by (tier, decision) (rate(ratelimit_decisions_total[5m]))", "{{tier}} {{decision}}")],
           "reqps", "The quota is per tier, which is the reason the limiter is "
                    "worth having rather than one global number.",
           x=0, y=17, stack=True, minv=0),
        ts("Caller resolution",
           [("sum by (result) (rate(auth_decisions_total[5m]))", "{{result}}")],
           "reqps", "authenticated, anonymous, rejected. The default mode never "
                    "rejects: auth here is a quota dimension, not a gate.",
           x=12, y=17, stack=True, minv=0),
        ts("Rate-limit buckets held", [("max(ratelimit_active_keys)", "buckets")], "short",
           "The one structure that would grow without bound if the TTL eviction "
           "ever broke.", x=0, y=25, minv=0),
        ts("Load shed", [("(sum(rate(http_server_inflight_rejected_total[5m])) or vector(0))", "shed")],
           "reqps", "The in-flight semaphore, which is the tier that actually "
                    "keeps the process alive under overload.",
           x=12, y=25, minv=0),

        row("Database", 33),
        ts("Pool connections",
           [('sum by (pool, state) (db_pool_connections{state=~"acquired|idle|constructing"})', "{{pool}} {{state}}"),
            ('max by (pool) (db_pool_connections{state="max"})', "{{pool}} max")],
           "short", "Separate pools per role are the bulkhead: a worker "
                    "saturated by slow jobs must not starve the API, and this is "
                    "where that shows.",
           x=0, y=34, minv=0),
        ts("Acquire wait",
           [("sum by (pool) (rate(db_pool_acquire_duration_seconds_total[5m])) / clamp_min(sum by (pool) (rate(db_pool_acquires_total{result=\"ok\"}[5m])), 1e-9)", "{{pool}} mean wait")],
           "s", "Mean rather than a quantile: pgxpool reports totals, so a "
                "distribution would need a tracer on every acquisition.",
           x=12, y=34, minv=0),

        row("Runtime", 42),
        ts("Goroutines", [("go_goroutines", "{{role}}")], "short", "", x=0, y=43, minv=0),
        ts("Heap in use", [("go_memstats_heap_inuse_bytes", "{{role}}")], "bytes", "", x=12, y=43, minv=0),
        ts("GC pause p99",
           [('histogram_quantile(0.99, sum by (le, role) (rate(go_gc_pauses_seconds_bucket[5m])))', "{{role}} max")], "s",
           "Turns 'it got slow' into 'it is in GC'.", x=0, y=51, minv=0),
        ts("Config reloads",
           [("sum by (result) (rate(config_reloads_total[15m]))", "{{result}}")],
           "ops", "A SIGHUP that was rejected is a change somebody thinks they "
                  "made.", x=12, y=51, minv=0),
    ],
    ["taskapi", "resilience"],
)

for name, d in [("api-red.json", api), ("queue-health.json", queue), ("resilience.json", res)]:
    (OUT / name).write_text(json.dumps(d, indent=2) + "\n")
    print(name, len(d["panels"]), "panels")
