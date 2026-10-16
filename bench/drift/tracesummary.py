#!/usr/bin/env python3
"""Summarise a TestLifeDrift trace (LIFE_DRIFT_TRACE) of the Game of Life.

Usage: python3 -I tracesummary.py TRACE.json[.gz] [...]

It reads the page's renderer main thread and reports what the trace shows
for the reader to check in DevTools or Perfetto: the main thread's tasks
(RunTask, as DevTools shows them) and the long ones (over 50 ms) with how
many board frames each applied (the lith:apply board measures that the
harness's traceMarks adds) and how many bytes of the stream each read
(lith:read <bytes> marks), the time between paints (main-thread Paint
events), the share of the main thread spent applying patch events
(Datastar's morph and what runs in it), and where the rest of the time
went (style, layout, paint, parsing, GC).
"""

import gzip
import json
import statistics
import sys


def load(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt") as f:
        d = json.load(f)
    return d["traceEvents"] if isinstance(d, dict) else d


def pct(xs, q):
    if not xs:
        return 0.0
    s = sorted(xs)
    return s[min(len(s) - 1, int(q * (len(s) - 1) + 0.5))]


def summarise(path):
    ev = load(path)
    threads = {}
    for e in ev:
        if e.get("ph") == "M" and e.get("name") == "thread_name":
            threads[(e["pid"], e["tid"])] = e["args"]["name"]
    # The renderer whose main thread has the harness's marks.
    pids = {e["pid"] for e in ev if str(e.get("name", "")).startswith("lith:")}
    mains = [k for k, v in threads.items() if v == "CrRendererMain" and k[0] in pids]
    if not mains:
        raise SystemExit(f"{path}: no renderer main thread with lith: marks")
    main = mains[0]
    on = [e for e in ev if (e.get("pid"), e.get("tid")) == main]
    xs = [e for e in on if e.get("ph") == "X" and "dur" in e]
    t0 = min(e["ts"] for e in xs)
    t1 = max(e["ts"] + e["dur"] for e in xs)
    span = (t1 - t0) / 1e6
    # Tasks as DevTools shows them: RunTask, and one still running when
    # the trace ended, up to its end.
    tasks = sorted((e["ts"], e["dur"]) for e in xs if e.get("name") == "RunTask")
    tasks += [(e["ts"], t1 - e["ts"]) for e in on if e.get("name") == "RunTask" and e.get("ph") == "B"]
    tasks.sort()

    # User Timing: reads, arrivals and applies.
    reads = [(e["ts"], int(e["name"].split()[1])) for e in on if e.get("name", "").startswith("lith:read ")]
    recv = sorted(e["ts"] for e in on if e.get("name") == "lith:recv board")
    begins, applies = {}, []
    for e in sorted((e for e in on if e.get("name", "").startswith("lith:apply ")), key=lambda e: e["ts"]):
        key = (e["name"], json.dumps(e.get("id2") or e.get("id")))
        if e["ph"] == "b":
            begins[key] = e["ts"]
        elif e["ph"] == "e" and key in begins:
            applies.append((e["name"][len("lith:apply ") :], begins.pop(key), e["ts"]))
    board = sorted((s, f) for n, s, f in applies if n == "board")

    def inside(ts, dur, points):
        return [p for p in points if ts <= p <= ts + dur]

    busy = sum(d for _, d in tasks) / 1e6
    long_tasks = sorted((t for t in tasks if t[1] > 50_000), key=lambda t: -t[1])
    rows = []
    for ts, dur in long_tasks:
        a = len(inside(ts, dur, [s for s, _ in board]))
        rs = [b for t, b in reads if ts <= t <= ts + dur]
        rows.append((dur / 1e3, a, len(rs), sum(rs), (ts - t0) / 1e6))
    per_task = [r[1] for r in rows if r[1] > 0]
    with_apply = []
    for ts, dur in tasks:
        a = len(inside(ts, dur, [s for s, _ in board]))
        if a:
            with_apply.append(a)

    # A rendering update paints each of its layers: one paint is the
    # Paint events less than 5 ms apart.
    paints = []
    for t in sorted(e["ts"] for e in on if e.get("name") == "Paint" and e.get("ph") == "X"):
        if not paints or t - paints[-1] > 5000:
            paints.append(t)
    gaps = [(b - a) / 1e3 for a, b in zip(paints, paints[1:])]
    # Frames applied between two paints: the frames a user could have seen
    # are at most one per paint.
    per_paint = []
    j = 0
    bs = [s for s, _ in board]
    for a, b in zip(paints, paints[1:]):
        n = 0
        while j < len(bs) and bs[j] < b:
            if bs[j] >= a:
                n += 1
            j += 1
        per_paint.append(n)

    apply_board = sum(f - s for s, f in board) / 1e6
    apply_all = sum(f - s for _, s, f in applies) / 1e6

    def total(*names):
        # Outermost events only, so a FunctionCall inside another counts once.
        n, end = 0, 0
        for e in sorted((e for e in xs if e.get("name") in names), key=lambda e: e["ts"]):
            if e["ts"] >= end:
                n += e["dur"]
                end = e["ts"] + e["dur"]
            elif e["ts"] + e["dur"] > end:
                n += e["ts"] + e["dur"] - end
                end = e["ts"] + e["dur"]
        return n / 1e6

    out = {
        "trace": path,
        "span_s": round(span, 2),
        "tasks": len(tasks),
        "busy_pct": round(100 * busy / span, 1),
        "long_tasks": len(long_tasks),
        "long_task_s": round(sum(d for _, d in long_tasks) / 1e6, 2),
        "longest_ms": [round(r[0]) for r in rows[:5]],
        "frames_applied": len(board),
        "frames_applied_per_s": round(len(board) / span, 2),
        "frames_arrived": len(recv),
        "frames_per_task_with_frames_mean": round(statistics.mean(with_apply), 2) if with_apply else 0,
        "frames_per_task_with_frames_max": max(with_apply) if with_apply else 0,
        "reads": len(reads),
        "read_bytes_median": pct([b for _, b in reads], 0.5),
        "read_bytes_max": max([b for _, b in reads], default=0),
        "read_bytes_min": min([b for _, b in reads], default=0),
        "paints": len(paints),
        "paint_gap_ms_median": round(pct(gaps, 0.5)),
        "paint_gap_ms_p90": round(pct(gaps, 0.9)),
        "paint_gap_ms_max": round(max(gaps, default=0)),
        "frames_per_paint_max": max(per_paint, default=0),
        "apply_board_pct": round(100 * apply_board / span, 1),
        "apply_all_pct": round(100 * apply_all / span, 1),
        "apply_board_ms_median": round(pct([(f - s) / 1e3 for s, f in board], 0.5), 1),
        "script_s": round(total("FunctionCall", "EvaluateScript", "RunMicrotasks"), 2),
        "parse_html_s": round(total("ParseHTML"), 2),
        "style_s": round(total("UpdateLayoutTree"), 2),
        "layout_s": round(total("Layout"), 2),
        "paint_s": round(total("Paint", "PrePaint", "Layerize", "Commit", "PaintImage"), 2),
        "gc_s": round(total("MinorGC", "MajorGC"), 2),
    }
    print(f"## {path}")
    print(
        f"{span:.1f} s of the main thread: {len(tasks)} tasks, {out['busy_pct']}% busy; "
        f"{len(long_tasks)} over 50 ms, {out['long_task_s']} s in all"
    )
    print("longest tasks (ms, board frames applied in it, stream reads in it, bytes read, starts at s):")
    for r in rows[:8]:
        print(f"  {r[0]:8.0f} ms  {r[1]:3d} frames  {r[2]} reads  {r[3]:>8d} B  at {r[4]:.1f} s")
    print(
        f"board frames: {len(recv)} arrived, {len(board)} applied ({out['frames_applied_per_s']}/s), "
        f"{out['frames_per_task_with_frames_mean']} per task that applied any (max {out['frames_per_task_with_frames_max']}); "
        f"applying one took {out['apply_board_ms_median']} ms at the median"
    )
    print(
        f"stream reads: {len(reads)}, bytes per read min {out['read_bytes_min']}, median {out['read_bytes_median']}, max {out['read_bytes_max']}"
    )
    print(
        f"paints: {len(paints)}, between two: median {out['paint_gap_ms_median']} ms, p90 {out['paint_gap_ms_p90']} ms, "
        f"max {out['paint_gap_ms_max']} ms; at most {out['frames_per_paint_max']} board frames applied between two paints"
    )
    print(
        f"applying patch events (Datastar's morph and what it runs): board {out['apply_board_pct']}%, all {out['apply_all_pct']}% of the time"
    )
    print(
        f"time in: script {out['script_s']} s (parse HTML {out['parse_html_s']} s), style {out['style_s']} s, "
        f"layout {out['layout_s']} s, paint {out['paint_s']} s, GC {out['gc_s']} s"
    )
    print("json " + json.dumps(out))
    print()


for p in sys.argv[1:]:
    summarise(p)
