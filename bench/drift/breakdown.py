#!/usr/bin/env python3
"""Where the main thread's time goes in a TestLifeDrift trace.

Usage: python3 -I breakdown.py TRACE.json[.gz] [...]

It splits the renderer main thread's tasks (RunTask) into those that
applied a board frame (a lith:apply board measure starts in them) and the
long ones (over 50 ms) that applied none, sums the outermost events in
each kind, and lists the functions that took the most time
(FunctionCall, by name and script) and what ran in requestAnimationFrame
callbacks.
"""

import collections
import gzip
import json
import sys


def main(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt") as f:
        d = json.load(f)["traceEvents"]
    th = {(e["pid"], e["tid"]): e["args"]["name"] for e in d if e.get("ph") == "M" and e.get("name") == "thread_name"}
    pids = {e["pid"] for e in d if str(e.get("name", "")).startswith("lith:")}
    main = [k for k, v in th.items() if v == "CrRendererMain" and k[0] in pids][0]
    on = [e for e in d if (e.get("pid"), e.get("tid")) == main]
    xs = sorted([e for e in on if e.get("ph") == "X" and "dur" in e], key=lambda e: (e["ts"], -e["dur"]))
    span = (max(e["ts"] + e["dur"] for e in xs) - min(e["ts"] for e in xs)) / 1e6
    tasks = [e for e in xs if e["name"] == "RunTask"]
    applies = sorted(e["ts"] for e in on if e.get("name") == "lith:apply board" and e["ph"] == "b")
    with_frame = [t for t in tasks if any(t["ts"] <= a <= t["ts"] + t["dur"] for a in applies)]
    long_without = [t for t in tasks if t["dur"] > 50000 and t not in with_frame]

    def children(sel):
        c, total = collections.Counter(), 0
        for t in sel:
            total += t["dur"]
            end, cur = t["ts"] + t["dur"], t["ts"]
            for e in xs:
                if e["ts"] < cur or e["ts"] >= end or e is t or e["name"] in ("RunTask", "ThreadControllerImpl::RunTask"):
                    continue
                c[e["name"]] += e["dur"]
                cur = e["ts"] + e["dur"]
        return total, c

    print(f"## {path}: {span:.1f} s, {len(tasks)} tasks, {len(with_frame)} applied a frame, {len(long_without)} long ones applied none")
    for name, sel in (("tasks that applied a frame", with_frame), ("long tasks without a frame", long_without), ("all tasks", tasks)):
        total, c = children(sel)
        print(f"{name}: {total / 1e6:.2f} s, {100 * total / 1e6 / span:.0f}% of the time")
        for k, v in c.most_common(8):
            print(f"    {k}: {v / 1e6:.2f} s ({100 * v / max(total, 1):.0f}%)")
    fc = collections.Counter()
    for e in xs:
        if e["name"] == "FunctionCall":
            a = e.get("args", {}).get("data", {})
            fc[(a.get("functionName") or "(anonymous)", a.get("url", "").rsplit("/", 1)[-1])] += e["dur"]
    print("function calls by time (outermost and nested alike):")
    for (fn, url), v in fc.most_common(6):
        print(f"    {v / 1e6:.2f} s  {fn} in {url}")
    print()


for p in sys.argv[1:]:
    main(p)
