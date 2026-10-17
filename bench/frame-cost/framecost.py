#!/usr/bin/env python3
"""What one board frame costs the page's main thread, from a TestLifeDrift
trace (LIFE_DRIFT_TRACE).

Usage: python3 -I framecost.py TRACE.json[.gz] [...]

For the renderer main thread that has the harness's lith: marks, it sums
the outermost events of each kind over the trace and divides by the board
frames applied in it (lith:apply board measures), so a frame's share of
the rendering counts even when several frames share one paint:

- apply: the lith:apply board measures, Datastar applying the patch event
  and what runs synchronously in it; split into parse HTML (ParseHTML),
  style and layout forced inside it, GC inside it, and the rest, which is
  script: Datastar's morph or, for a signal patch, the effects it runs.
- mo: Datastar's MutationObserver callback (FunctionCall of
  Co.MutationObserver...), which scans the nodes a morph added.
- style, layout, paint (Paint and PaintImage), raster prep (PrePaint,
  Layerize, Commit, UpdateLayer), raf (FireAnimationFrame, the
  sparkline's), gc, all outside the apply measures.
- busy: all tasks (RunTask); other is busy less everything above.

Times are milliseconds per frame applied. The last columns are the frames
applied a second in the trace, and of them the frames shown a second: a
frame counts as shown when the main thread commits to the compositor
after it and before the next frame is applied.
"""

import gzip
import json
import os
import sys


def load(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt") as f:
        d = json.load(f)
    return d["traceEvents"] if isinstance(d, dict) else d


def union(iv):
    out = []
    for s, e in sorted(iv):
        if out and s <= out[-1][1]:
            out[-1][1] = max(out[-1][1], e)
        else:
            out.append([s, e])
    return out


def length(iv):
    return sum(e - s for s, e in iv)


def intersect(a, b):
    out, i, j = [], 0, 0
    while i < len(a) and j < len(b):
        s, e = max(a[i][0], b[j][0]), min(a[i][1], b[j][1])
        if s < e:
            out.append([s, e])
        if a[i][1] < b[j][1]:
            i += 1
        else:
            j += 1
    return out


def subtract(a, b):
    return length(a) - length(intersect(a, b))


def main(path):
    ev = load(path)
    th = {(e["pid"], e["tid"]): e["args"]["name"] for e in ev if e.get("ph") == "M" and e.get("name") == "thread_name"}
    pids = {e["pid"] for e in ev if str(e.get("name", "")).startswith("lith:")}
    main = [k for k, v in th.items() if v == "CrRendererMain" and k[0] in pids][0]
    on = [e for e in ev if (e.get("pid"), e.get("tid")) == main]
    xs = [e for e in on if e.get("ph") == "X" and "dur" in e]
    t0 = min(e["ts"] for e in xs)
    t1 = max(e["ts"] + e["dur"] for e in xs)
    span = (t1 - t0) / 1e6

    def iv(*names, fn=None):
        return union([[e["ts"], e["ts"] + e["dur"]] for e in xs if e["name"] in names and (fn is None or fn(e))])

    begins, applies = {}, []
    for e in sorted((e for e in on if e.get("name") == "lith:apply board"), key=lambda e: e["ts"]):
        key = json.dumps(e.get("id2") or e.get("id"))
        if e["ph"] == "b":
            begins[key] = e["ts"]
        elif e["ph"] == "e" and key in begins:
            applies.append([begins.pop(key), e["ts"]])
    apply = union(applies)
    n = max(len(applies), 1)
    mo = iv("FunctionCall", fn=lambda e: "MutationObserver" in (e.get("args", {}).get("data", {}).get("functionName") or ""))
    parse = iv("ParseHTML")
    style = iv("UpdateLayoutTree")
    layout = iv("Layout")
    gc = iv("MinorGC", "MajorGC")
    paint = iv("Paint", "PaintImage")
    prep = iv("PrePaint", "Layerize", "Commit", "UpdateLayer")
    raf = iv("FireAnimationFrame")
    busy = iv("RunTask")
    a = {
        "apply": length(apply),
        "parse": length(intersect(parse, apply)),
        "forced": length(intersect(union(style + layout), apply)),
        "gc_in": length(intersect(gc, apply)),
    }
    a["script"] = a["apply"] - a["parse"] - a["forced"] - a["gc_in"]
    rest = {
        "mo": subtract(mo, apply),
        "style": subtract(style, apply),
        "layout": subtract(layout, apply),
        "paint": subtract(paint, apply),
        "prep": subtract(prep, apply),
        "raf": subtract(raf, apply),
        "gc": subtract(gc, apply),
    }
    known = union(apply + mo + style + layout + paint + prep + raf + gc)
    rest["other"] = length(busy) - length(intersect(known, busy))
    row = {k: v / 1e3 / n for k, v in {**a, **rest}.items()}
    row["busy"] = length(busy) / 1e3 / n
    row["busy_pct"] = 100 * length(busy) / 1e6 / span
    row["fps"] = len(applies) / span
    # A frame is shown when a commit to the compositor follows it before
    # the next frame is applied; a canvas paints without a Paint event on
    # the main thread, so commits count, not paints.
    commits = sorted(e["ts"] for e in xs if e["name"] == "Commit")
    ends = sorted(f for _, f in applies)
    shown, j = 0, 0
    for k, f in enumerate(ends):
        nxt = ends[k + 1] if k + 1 < len(ends) else t1
        while j < len(commits) and commits[j] < f:
            j += 1
        if j < len(commits) and commits[j] < nxt:
            shown += 1
    row["shown"] = shown / span
    name = os.path.basename(path).split(".trace")[0]
    print(
        f"| {name} | {row['apply']:.1f} | {row['parse']:.1f} | {row['script']:.1f} | {row['forced']:.1f} | {row['mo']:.1f} | "
        f"{row['style']:.1f} | {row['layout']:.1f} | {row['paint']:.1f} | {row['prep']:.1f} | {row['raf']:.1f} | "
        f"{row['gc'] + row['gc_in']:.1f} | {row['other']:.1f} | {row['busy']:.1f} | {row['busy_pct']:.0f}% | {row['fps']:.2f} | {row['shown']:.2f} |"
    )


print("| Trace | Apply | of it: parse HTML | of it: script (morph, effect) | of it: forced style/layout | Datastar MutationObserver | Style | Layout | Paint | Pre-paint, layerize, commit | rAF (sparkline) | GC | Other | Busy per frame | Busy | Frames/s | Shown/s |")
print("| --- | " + " | ".join(["---"] * 16) + " |")
for p in sys.argv[1:]:
    main(p)
