#!/usr/bin/env python3
"""Cut a window out of a TestLifeDrift trace for a screenshot.

Usage: python3 -I crop.py IN.json[.gz] OUT.json START LENGTH

START and LENGTH are seconds, START from the first event of the page's
renderer main thread. It keeps the metadata and the events that start in
the window, and leaves out the CPU profile, whose samples count from its
start and would stretch the timeline: the flame chart then shows the
trace's own events (tasks, function calls, event dispatch, style,
layout, paint, GC and the lith: User Timing marks).
"""

import gzip
import json
import sys

src, dst, start, length = sys.argv[1], sys.argv[2], float(sys.argv[3]), float(sys.argv[4])
opener = gzip.open if src.endswith(".gz") else open
with opener(src, "rt") as f:
    d = json.load(f)
ev = d["traceEvents"]
threads = {(e["pid"], e["tid"]): e["args"]["name"] for e in ev if e.get("ph") == "M" and e.get("name") == "thread_name"}
pids = {e["pid"] for e in ev if str(e.get("name", "")).startswith("lith:")}
main = [k for k, v in threads.items() if v == "CrRendererMain" and k[0] in pids][0]
t0 = min(e["ts"] for e in ev if (e.get("pid"), e.get("tid")) == main and e.get("ph") == "X")
lo, hi = t0 + start * 1e6, t0 + (start + length) * 1e6
KEEP = {"TracingStartedInBrowser", "TracingStartedInPage", "SetLayerTreeId", "FrameCommittedInBrowser", "TracingSessionIdForWorker"}
out = []
for e in ev:
    if e.get("name") in ("Profile", "ProfileChunk"):
        continue
    if e.get("name") in KEEP and e.get("ts", 0) <= hi:
        # What tells DevTools which renderer and frame are the page's.
        out.append(dict(e, ts=max(e["ts"], int(lo))))
        continue
    # Async events whose other end lies outside the window would stretch
    # the timeline; the User Timing measures stay.
    if e.get("ph") in ("b", "e", "n", "S", "F", "T", "p") and "user_timing" not in e.get("cat", ""):
        continue
    if e.get("ph") == "M" or lo <= e.get("ts", 0) <= hi:
        if e.get("ph") == "X" and e["ts"] + e.get("dur", 0) > hi:
            e = dict(e, dur=int(hi - e["ts"]))
        out.append(e)
d["traceEvents"] = out
with open(dst, "w") as f:
    json.dump(d, f)
print(f"{len(out)} events, {start} s to {start + length} s")
