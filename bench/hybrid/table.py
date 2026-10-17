#!/usr/bin/env python3
"""Tabulate the hybrid's TestLifeDrift runs from run.sh's outputs.

Usage:
  python3 -I table.py runs PREFIX [...]
      One row per configuration, from NAME-1.log, NAME-2.log ... or NAME.log:
      the median of the runs and their range.
  python3 -I table.py phases SECONDS[,SECONDS...] NAME [...]
      One row per run and phase, the phases split at the seconds given,
      from NAME.csv and NAME.lag.csv.

The switch is when the page first asked for pacing: its POST /pace, or
its stream opened again with Save-Data: on. "Around the switch" is the
10 s from 5 s before it to 5 s after.
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


def lagcsv(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            rows.append({k: float(v) for k, v in r.items()})
    return rows


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
    pace = PACE.search(text)
    slow = SLOW.search(text)
    sw = None
    if pace:
        r["posts"] = len(pace.group(1).split(","))
        sw = float(pace.group(1).split(",")[0])
    elif slow:
        sw = float(slow.group(1))
    hub = HUB.findall(text)
    r["paces"] = int(hub[-1][1]) if hub else 0
    r["releases"] = int(hub[-1][2]) if hub else 0
    rel = [int(t) for t, _, n in hub if int(n) > 0]
    if sw is not None:
        r["switch"] = sw
        lag = lagcsv(log[: -len(".log")] + ".lag.csv")
        around = [x["lag_s"] for x in lag if sw - 5 <= x["t"] <= sw + 5]
        if around:
            r["around_med"], r["around_max"] = statistics.median(around), max(around)
    return r


def cell(xs, fmt):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "n/a"
    m = statistics.median(xs)
    if len(xs) == 1 or min(xs) == max(xs):
        return fmt.format(m)
    return (fmt + " ({}-{})").format(m, fmt.format(min(xs)), fmt.format(max(xs)))


def runs(prefixes):
    print(
        "| Configuration | Runs | Lag median (s) | Lag max (s) | Switch at (s) | Lag around switch, median / max (s) | "
        "Applied/s | Painted/s | Site wrote KB/s | Acks/s | Site CPU | Caddy CPU | Link up / down KB/s | Paced, released | Load |"
    )
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for p in prefixes:
        files = sorted(glob.glob(p + "-[0-9].log")) or ([p + ".log"] if os.path.exists(p + ".log") else [])
        rs = [r for r in (parse(f) for f in files) if r]
        if not rs:
            continue
        col = lambda k: [r.get(k) for r in rs]
        link = "n/a"
        if rs[0].get("up") is not None:
            link = cell(col("up"), "{:.2f}") + " / " + cell(col("down"), "{:.2f}")
        around = cell(col("around_med"), "{:.2f}") + " / " + cell(col("around_max"), "{:.2f}")
        pr = ", ".join(sorted({f"{r['paces']}/{r['releases']}" for r in rs}))
        print(
            f"| {os.path.basename(p)} | {len(rs)} | {cell(col('lag'), '{:.2f}')} | {cell(col('lagmax'), '{:.2f}')} | "
            f"{cell(col('switch'), '{:.1f}')} | {around} | {cell(col('applied'), '{:.2f}')} | {cell(col('painted'), '{:.2f}')} | "
            f"{cell(col('kbs'), '{:.2f}')} | {cell(col('acks_s'), '{:.2f}')} | {cell(col('site'), '{:.1f}')}% | "
            f"{cell(col('caddy'), '{:.2f}')}% | {link} | {pr} | {cell(col('load'), '{:.1f}')} |"
        )


def phases(cuts, names):
    print(
        "| Run | Phase (s) | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Site CPU | Caddy CPU | Events |"
    )
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for n in names:
        text = open(n + ".log").read()
        lag = lagcsv(n + ".lag.csv")
        rows = []
        with open(n + ".csv") as f:
            for r in csv.DictReader(f):
                rows.append({k: float(v) for k, v in r.items()})
        end = int(rows[-1]["t"] + 0.5)
        bounds = [0] + cuts + [end]
        events = []
        sl = SLOW.search(text)
        if sl:
            events.append(f"Save-Data stream at {float(sl.group(1)):.1f} s")
        pc = PACE.search(text)
        if pc:
            events.append(f"/pace at {pc.group(1)} s")
        last = (0, 0)
        for t, p, rl in HUB.findall(text):
            if (int(p), int(rl)) != last:
                what = "paced" if int(p) > last[0] else "released"
                events.append(f"{what} by {t} s")
                last = (int(p), int(rl))
        for a, b in zip(bounds, bounds[1:]):
            ls = [x for x in lag if a < x["t"] <= b]
            rs = [x for x in rows if a < x["t"] <= b + 0.5]
            secs = b - a
            prev = [x for x in rows if x["t"] <= a + 0.5]
            p0 = prev[-1] if prev else {k: 0.0 for k in rows[0]}
            p1 = rs[-1]
            kbs = (p1["wrote"] - p0["wrote"]) / 1000 / secs
            acks = (p1["acks"] - p0["acks"]) / secs
            site = sum(x["site_cpu_ms"] for x in rs) / 10 / secs
            caddy = sum(x["caddy_cpu_ms"] for x in rs) / 10 / secs
            print(
                f"| {os.path.basename(n)} | {a}-{b} | {statistics.median(x['lag_s'] for x in ls):.2f} | {max(x['lag_s'] for x in ls):.2f} | "
                f"{statistics.mean(x['applied'] for x in ls):.2f} | {statistics.mean(x['painted'] for x in ls):.2f} | {kbs:.2f} | {acks:.2f} | "
                f"{site:.1f}% | {caddy:.2f}% | {'; '.join(events) if a == 0 else ''} |"
            )


if sys.argv[1] == "runs":
    runs(sys.argv[2:])
elif sys.argv[1] == "phases":
    phases([int(x) for x in sys.argv[2].split(",")], sys.argv[3:])
