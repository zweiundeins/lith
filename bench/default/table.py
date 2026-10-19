#!/usr/bin/env python3
"""Tabulate the runs in bench/default from run.sh's outputs.

Usage:
  python3 -I table.py runs PREFIX [...]
      One row per configuration, from NAME-1.log, NAME-2.log ...: the
      median of the runs and their range.
  python3 -I table.py phases SECONDS[,SECONDS...] PREFIX [...]
      One row per configuration and phase, the phases split at the seconds
      given, the median of the runs. "Drained" is how long after the first
      cut the lag first fell under 0.2 s.
  python3 -I table.py input PREFIX [...]
      Input responsiveness, from NAME-N.input.csv and the logs: the time
      from sending a key press to the page's handler and to DevTools'
      reply, over the presses of all runs, and Event Timing's keydown.
  python3 -I table.py load LOG [...]
      TestHybridLoad's rows, the median of the rounds and their range.
"""

import csv
import glob
import os
import re
import statistics
import sys

SUMMARY = re.compile(
    r"summary: lag median (?P<lag>[\d.]+) s, max (?P<lagmax>[\d.]+) s; applied (?P<applied>[\d.]+) frames/s, painted (?P<painted>[\d.]+); "
    r"main thread busy (?P<busy>[\d.]+)%; site CPU (?P<site>[\d.]+)%, Caddy CPU (?P<caddy>[\d.]+)%; "
    r"lith wrote (?P<kbs>[\d.]+) KB/s; (?P<sent>\d+) frames sent, (?P<dropped>\d+) dropped, (?P<deferred>\d+) renders deferred; "
    r"(?P<reqs>[\d.]+) other requests/s, (?P<reqb>[\d.]+) B each; (?P<acks>\d+) acks"
)
LINK = re.compile(r"link: (?P<up>[\d.]+) KB/s up, (?P<down>[\d.]+) KB/s down")
LOAD = re.compile(r"load average: ([\d.]+), ([\d.]+), ([\d.]+)")
SLOW = re.compile(r"a slow stream \(Save-Data\) after ([\d.]+) s")
PACE = re.compile(r"the page posted /pace at ([\d., ]+) s")
HUB = re.compile(r"hub at (\d+) s: (\d+) paced, (\d+) released")
FOR = re.compile(r"LIFE_DRIFT_FOR=(\d+)s")
RTT = re.compile(r"reported round trips of (\d+) ms at least, (\d+) ms at the median")
INPUT = re.compile(
    r"input: (?P<n>\d+) presses, (?P<handled>\d+) handled, (?P<pending>\d+) not by the end; .*?"
    r"Event Timing keydown (?P<entries>\d+) of 16 ms or more, p98 (?P<p98>\d+) ms, max (?P<etmax>\d+) ms, input delay max (?P<delay>\d+) ms"
)
LOADROW = re.compile(r"\| (\w+) \| (\d+) \| ([\d.]+) \| (\d+) \| (\d+) \| (\d+) \| (\d+) \| ([\d.]+) \| ([\d.]+) \|")


def floats(path):
    with open(path) as f:
        return [{k: float(v) if v != "" else None for k, v in r.items()} for r in csv.DictReader(f)]


def files(prefix):
    return sorted(glob.glob(prefix + "-[0-9].log")) or ([prefix + ".log"] if os.path.exists(prefix + ".log") else [])


def events(text):
    """The switch (first /pace or Save-Data stream), the hub's paces and
    releases, and when the first release came."""
    r = {}
    pace, slow = PACE.search(text), SLOW.search(text)
    if pace:
        r["switch"] = float(pace.group(1).split(",")[0])
        r["posts"] = [float(x) for x in pace.group(1).split(",")]
    elif slow:
        r["switch"] = float(slow.group(1))
    hub = HUB.findall(text)
    r["paces"] = int(hub[-1][1]) if hub else 0
    r["releases"] = int(hub[-1][2]) if hub else 0
    rel = [int(t) for t, _, n in hub if int(n) > 0]
    if rel:
        r["released_at"] = rel[0]
    return r


def parse(log):
    text = open(log).read()
    m = SUMMARY.search(text)
    if not m:
        return None
    r = {k: float(v) for k, v in m.groupdict().items()}
    secs = float(FOR.search(text).group(1))
    r["acks_s"] = r["acks"] / secs
    lk = LINK.search(text)
    if lk:
        r["up"], r["down"] = float(lk.group("up")), float(lk.group("down"))
    r["load"] = float(LOAD.search(text).group(1))
    rt = RTT.search(text)
    if rt:
        r["rtt_min"], r["rtt_med"] = float(rt.group(1)), float(rt.group(2))
    r.update(events(text))
    inp = INPUT.search(text)
    if inp:
        r.update({"in_" + k: float(v) for k, v in inp.groupdict().items()})
    return r


def cell(xs, fmt):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "n/a"
    m = statistics.median(xs)
    if len(xs) == 1 or min(xs) == max(xs):
        return fmt.format(m)
    return (fmt + " ({}-{})").format(m, fmt.format(min(xs)), fmt.format(max(xs)))


def switched(rs):
    """Paced/released per run, and when the first release came."""
    out = []
    for r in rs:
        s = f"{r['paces']}/{r['releases']}"
        if "released_at" in r:
            s += f" at {r['released_at']}"
        out.append(s)
    return ", ".join(out)


def runs(prefixes):
    print(
        "| Configuration | Runs | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | "
        "Link up / down KB/s | Switch at (s) | Paced/released by run | Site CPU | Caddy CPU | Load |"
    )
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for p in prefixes:
        rs = [r for r in (parse(f) for f in files(p)) if r]
        if not rs:
            continue
        col = lambda k: [r.get(k) for r in rs]
        link = "n/a"
        if rs[0].get("up") is not None:
            link = cell(col("up"), "{:.2f}") + " / " + cell(col("down"), "{:.2f}")
        sw = cell(col("switch"), "{:.1f}") if any(r.get("switch") is not None for r in rs) else "never"
        if any(r.get("switch") is None for r in rs) and sw != "never":
            sw += f" ({sum(r.get('switch') is not None for r in rs)} of {len(rs)})"
        print(
            f"| {os.path.basename(p)} | {len(rs)} | {cell(col('lag'), '{:.2f}')} | {cell(col('lagmax'), '{:.2f}')} | "
            f"{cell(col('applied'), '{:.2f}')} | {cell(col('painted'), '{:.2f}')} | {cell(col('kbs'), '{:.2f}')} | "
            f"{cell(col('acks_s'), '{:.2f}')} | {link} | {sw} | {switched(rs)} | {cell(col('site'), '{:.1f}')}% | "
            f"{cell(col('caddy'), '{:.1f}')}% | {cell(col('load'), '{:.1f}')} |"
        )


def phase_rows(name, cuts):
    lag = floats(name + ".lag.csv")
    rows = floats(name + ".csv")
    end = int(rows[-1]["t"] + 0.5)
    bounds = [0] + cuts + [end]
    out = []
    for a, b in zip(bounds, bounds[1:]):
        ls = [x for x in lag if a < x["t"] <= b]
        rs = [x for x in rows if a < x["t"] <= b + 0.5]
        prev = [x for x in rows if x["t"] <= a + 0.5]
        p0 = prev[-1] if prev else {k: 0.0 for k in rows[0]}
        p1 = rs[-1]
        secs = b - a
        out.append(
            {
                "phase": f"{a}-{b}",
                "lag": statistics.median(x["lag_s"] for x in ls),
                "lagmax": max(x["lag_s"] for x in ls),
                "applied": statistics.mean(x["applied"] for x in ls),
                "painted": statistics.mean(x["painted"] for x in ls),
                "kbs": (p1["wrote"] - p0["wrote"]) / 1000 / secs,
                "acks": (p1["acks"] - p0["acks"]) / secs,
            }
        )
    drained, at_cut = None, None
    if cuts:
        c = cuts[0]
        at = [x for x in lag if x["t"] == c]
        at_cut = at[0]["lag_s"] if at else None
        for x in lag:
            if x["t"] > c and x["lag_s"] < 0.2:
                drained = x["t"] - c
                break
    return out, drained, at_cut


def phases(cuts, prefixes):
    print("| Configuration | Phase (s) | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Lag at the cut (s) | Drained after (s) | Switch at (s) | Paced/released by run |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for p in prefixes:
        logs = files(p)
        names = [f[: -len(".log")] for f in logs]
        per = [phase_rows(n, cuts) for n in names]
        rs = [r for r in (parse(f) for f in logs) if r]
        for i, ph in enumerate(per[0][0]):
            col = lambda k: [x[0][i][k] for x in per]
            first = i == 0
            drained = ""
            atcut = ""
            sw = ""
            if first:
                atcut = cell([x[2] for x in per], "{:.2f}")
                ds = [x[1] for x in per]
                drained = cell(ds, "{:.0f}") if all(d is not None for d in ds) else ", ".join("never" if d is None else f"{d:.0f}" for d in ds)
                sw = cell([r.get("switch") for r in rs], "{:.1f}") if any(r.get("switch") is not None for r in rs) else "never"
            print(
                f"| {os.path.basename(p) if first else ''} | {ph['phase']} | {cell(col('lag'), '{:.2f}')} | {cell(col('lagmax'), '{:.2f}')} | "
                f"{cell(col('applied'), '{:.2f}')} | {cell(col('painted'), '{:.2f}')} | {cell(col('kbs'), '{:.2f}')} | {cell(col('acks'), '{:.2f}')} | "
                f"{atcut} | {drained} | {sw} | {switched(rs) if first else ''} |"
            )


def pct(xs, p):
    s = sorted(xs)
    if not s:
        return float("nan")
    i = max(0, min(len(s) - 1, int(-(-len(s) * p // 100)) - 1))
    return s[i]


def inputs(prefixes):
    print(
        "| Configuration | Runs | Presses | To the handler: median / p95 / max (ms) | DevTools' dispatch: median / p95 / max (ms) | "
        "Event Timing keydown p98 / max (ms) | Lag median (s) | Applied/s | Load |"
    )
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for p in prefixes:
        logs = files(p)
        rs = [r for r in (parse(f) for f in logs) if r]
        handler, dispatch = [], []
        for f in logs:
            for x in floats(f[: -len(".log")] + ".input.csv"):
                if x["handler_ms"] is not None:
                    handler.append(x["handler_ms"])
                if x["dispatch_ms"] is not None:
                    dispatch.append(x["dispatch_ms"])
        col = lambda k: [r.get(k) for r in rs]
        print(
            f"| {os.path.basename(p)} | {len(rs)} | {len(handler)} | {pct(handler, 50):.0f} / {pct(handler, 95):.0f} / {max(handler):.0f} | "
            f"{pct(dispatch, 50):.0f} / {pct(dispatch, 95):.0f} / {max(dispatch):.0f} | "
            f"{cell(col('in_p98'), '{:.0f}')} / {max(col('in_etmax')):.0f} | {cell(col('lag'), '{:.2f}')} | {cell(col('applied'), '{:.2f}')} | {cell(col('load'), '{:.1f}')} |"
        )


def load(logs):
    print("| Log | Streams | Mode | CPU (% of a core) | Acks/s | Wire KB/s | Frames sent/s | Frames dropped/s | Renders deferred/s | Load |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for log in logs:
        text = open(log).read()
        ld = LOAD.search(text).group(1)
        rows = {}
        for m in LOADROW.finditer(text):
            mode, n = m.group(1), int(m.group(2))
            rows.setdefault((n, mode), []).append([float(x) for x in m.groups()[2:]])
        for (n, mode), xs in rows.items():
            c = lambda i, fmt: cell([x[i] for x in xs], fmt)
            print(
                f"| {os.path.basename(log)} | {n:,} | {mode} | {c(0, '{:.0f}')}% | {c(1, '{:.0f}')} | {c(2, '{:.0f}')} | {c(3, '{:.0f}')} | "
                f"{c(4, '{:.0f}')} | {c(6, '{:.1f}')} | {ld} |"
            )


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "runs":
        runs(sys.argv[2:])
    elif cmd == "phases":
        phases([int(x) for x in sys.argv[2].split(",")], sys.argv[3:])
    elif cmd == "input":
        inputs(sys.argv[2:])
    elif cmd == "load":
        load(sys.argv[2:])
