#!/usr/bin/env python3
"""Show a TestLifeDrift trace second by second around the switch.

Usage: python3 -I switch.py TRACE.json[.gz]

The switch is the page's lith:behind mark (newest-frame-behind). For each
second from 8 s before it to 10 s after, on the page's renderer main
thread: the share of the second in tasks, the longest task, the board
frames applied (lith:apply board), the paints, the reads of the stream
and their bytes, and the requests the page sent to /ack and /pace or for
its stream (ResourceSendRequest). Times are from the switch.
"""

import gzip
import json
import sys
from collections import defaultdict

path = sys.argv[1]
opener = gzip.open if path.endswith(".gz") else open
with opener(path, "rt") as f:
    d = json.load(f)
ev = d["traceEvents"] if isinstance(d, dict) else d
threads = {(e["pid"], e["tid"]): e["args"]["name"] for e in ev if e.get("ph") == "M" and e.get("name") == "thread_name"}
pids = {e["pid"] for e in ev if str(e.get("name", "")).startswith("lith:")}
main = [k for k, v in threads.items() if v == "CrRendererMain" and k[0] in pids][0]
on_main = lambda e: (e.get("pid"), e.get("tid")) == main

behind = sorted(e["ts"] for e in ev if e.get("name") == "lith:behind" and e.get("ph") in ("R", "I", "i", "n", "b"))
if not behind:
    sys.exit("no lith:behind mark")
t0 = behind[0]
t_first = min(e["ts"] for e in ev if on_main(e) and e.get("ph") == "X")
print(f"switch (lith:behind) at {(t0 - t_first) / 1e6:.2f} s into the trace; {len(behind)} lith:behind marks")

bins = defaultdict(lambda: defaultdict(float))
lo, hi = -8, 10


def b(ts):
    s = int((ts - t0) // 1_000_000)
    return s if lo <= s < hi else None


for e in ev:
    name, ph, ts = e.get("name", ""), e.get("ph"), e.get("ts", 0)
    k = b(ts)
    if k is None:
        continue
    if on_main(e) and ph == "X" and name == "RunTask":
        bins[k]["busy"] += e.get("dur", 0) / 1e4  # percent of a second
        bins[k]["longest"] = max(bins[k]["longest"], e.get("dur", 0) / 1e3)
    elif on_main(e) and ph == "X" and name == "Paint":
        bins[k]["paints"] += 1
    elif name == "lith:apply board" and ph in ("b", "X"):
        bins[k]["applied"] += 1
    elif name.startswith("lith:read ") and ph in ("R", "I", "i", "n", "b"):
        bins[k]["reads"] += 1
        bins[k]["bytes"] += int(name.split()[1])
    elif name == "ResourceSendRequest":
        url = e.get("args", {}).get("data", {}).get("url", "")
        if "/ack?" in url:
            bins[k]["acks"] += 1
        elif "/pace?" in url:
            bins[k]["pace"] += 1
        elif "/stream?" in url:
            bins[k]["stream"] += 1

print("| s from switch | main thread busy | longest task (ms) | frames applied | paints | stream reads (KB) | /ack | /pace |")
print("| --- | --- | --- | --- | --- | --- | --- | --- |")
for k in range(lo, hi):
    x = bins[k]
    print(
        f"| {k} | {x['busy']:.0f}% | {x['longest']:.0f} | {x['applied']:.0f} | {x['paints']:.0f} | "
        f"{x['reads']:.0f} ({x['bytes'] / 1000:.0f}) | {x['acks']:.0f} | {x['pace']:.0f} |"
    )
