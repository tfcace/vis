#!/usr/bin/env python3
"""One line of a scenario timeline: what clients, vis and the app see right now.
It reads vis's own API (/api/state, the contract the control UI uses) and the load generator."""
import json, os, sys, urllib.request

UI = os.environ.get("CACHEPROXY_UI", "http://127.0.0.1:7071")
L = os.environ.get("LOADGEN", "http://127.0.0.1:8792")
get = lambda base, p: json.load(urllib.request.urlopen(base + p, timeout=8))
c = get(L, "/stats")  # what clients saw in the last 10 s, from the load generator
head = (f'{sys.argv[1]:<14} client p99={c["p99_ms"]:6.1f}ms ok={c["ok"]:5d} fail={c["failed"]:4d} shed={c["shed"]:4d} '
        f'limited={c.get("limited", 0):4d}')
try:
    s = get(UI, "/api/state")
except OSError as e:  # the service is too far gone to answer: what clients saw still counts
    print(f"{head} | vis: no answer ({e})", flush=True)
    sys.exit(0)

def fleet_part():
    """With FLEET=n, each replica: what clients saw from it, and its own CPU and compress_level."""
    n = int(os.environ.get("FLEET") or 0)
    parts = []
    for i in range(1, n + 1):
        t = (c.get("targets") or {}).get(f"http://cp-{i}:8790", {})
        try:
            r = get(f"http://127.0.0.1:{7071 + 10 * (i - 1)}", "/api/state")
            k = {x["name"]: x for x in r["knobs"]}
            own = f'throttled={r["runtime"]["throttled"]:.0%} compress={k["compress_level"]["value"]}'
        except OSError:
            own = "no answer"
        parts.append(f'cp-{i}: client p99={t.get("p99_ms", 0):.1f}ms ok={t.get("ok", 0)} fail={t.get("failed", 0)} shed={t.get("shed", 0)} {own}')
    return " || " + " | ".join(parts) if parts else ""

route = next((r for r in s["routes"] or [] if r["route"] == "GET /k/{key}"), None)
lat = route["history"][-2] if route and len(route["history"]) > 1 else {}  # the last closed 10 s bucket
rt = s["runtime"]
sig = {x["name"]: x["value"] for x in s["signals"] or []}
knob = {k["name"]: k for k in s["knobs"]}

def show(name):
    k = knob[name]
    by = k["stack"][0].get("by")
    return f'{k["value"]}' + (f' ({by})' if by else "")

rules = ", ".join(r["state"] + (" (%d stalls)" % r["stalls"] if r["stalls"] else "") for r in s["rules"] or []) or "-"
print(f'{head} '
      f'| server p99={lat.get("p99_ms", 0):6.1f}ms '
      f'| sched_p99={rt["sched_p99_ms"]:6.2f}ms gc={rt["gc_cpu"]:.3f} '
      f'cpu={rt["cpu_used"]:.2f}/{rt["cpu_limit"] or "-"} throttled={rt["throttled"]:.0%} '
      f'| hit={sig.get("hit_rate", 0):.2f} '
      f'| batch={show("batch_size")} compress={show("compress_level")} | rule: {rules}'
      + fleet_part(), flush=True)
