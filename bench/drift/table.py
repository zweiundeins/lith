#!/usr/bin/env python3
"""Tabulate TestLifeDrift runs from their logs (run.sh NAME.log).

Usage: python3 -I table.py PREFIX [...]

For each configuration, the logs NAME-1.log, NAME-2.log ... (or a single
NAME.log) are one run each. It prints a Markdown table with the median of
the runs and their range: the lag behind the board (each run's median
and maximum over every second), the lag at the end, frames applied a
second, the main thread's share of busy time, the CPU of the site's
process and of Caddy, what the site wrote, the acknowledgements a second,
the bytes on the link when there is one, and the load average before the
runs.
"""

import glob
import os
import re
import statistics
import sys

SUMMARY = re.compile(
    r"summary: lag median (?P<lag>[\d.]+) s, max (?P<lagmax>[\d.]+) s; applied (?P<applied>[\d.]+) frames/s; "
    r"main thread busy (?P<busy>[\d.]+)%; site CPU (?P<site>[\d.]+)%, Caddy CPU (?P<caddy>[\d.]+)%; "
    r"lith wrote (?P<kbs>[\d.]+) KB/s; (?P<sent>\d+) frames sent, (?P<dropped>\d+) dropped, (?P<deferred>\d+) renders deferred; "
    r"(?P<reqs>[\d.]+) other requests/s, (?P<reqb>[\d.]+) B each(; (?P<acks>\d+) acks)?"
)
LINK = re.compile(r"link: (?P<up>[\d.]+) KB/s up, (?P<down>[\d.]+) KB/s down")
LOAD = re.compile(r"load average: ([\d.]+), ([\d.]+), ([\d.]+)")
BEHIND = re.compile(r"behind the board after (\d+) s: ([\d.]+) s")
SLOW = re.compile(r"a slow stream \(Save-Data\) (.*)$")


def parse(path):
    text = open(path).read()
    m = SUMMARY.search(text)
    if not m:
        return None
    r = {k: float(v) for k, v in m.groupdict().items() if v is not None and k != "acks"}
    r["acks"] = float(m.group("acks") or 0)
    lk = LINK.search(text)
    if lk:
        r["up"], r["down"] = float(lk.group("up")), float(lk.group("down"))
    ld = LOAD.search(text)
    r["load"] = float(ld.group(1)) if ld else 0.0
    behind = BEHIND.findall(text)
    r["end"] = float(behind[-1][1]) if behind else 0.0
    sl = SLOW.search(text)
    r["slow"] = sl.group(1).strip() if sl else ""
    return r


def cell(xs, fmt):
    if not xs:
        return ""
    m = statistics.median(xs)
    if len(xs) == 1 or min(xs) == max(xs):
        return fmt.format(m)
    return (fmt + " ({}-{})").format(m, fmt.format(min(xs)), fmt.format(max(xs)))


def main(prefixes):
    print(
        "| Configuration | Runs | Lag median (s) | Lag max (s) | Lag at end (s) | Applied/s | Main thread busy | "
        "Site CPU | Caddy CPU | Site wrote KB/s | Acks/s | Link up/down KB/s | Load |"
    )
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for p in prefixes:
        files = sorted(glob.glob(p + "-[0-9].log")) or ([p + ".log"] if os.path.exists(p + ".log") else [])
        rs = [r for r in (parse(f) for f in files) if r]
        if not rs:
            continue
        secs = 1.0
        col = lambda k: [r[k] for r in rs if k in r]
        acks = []
        for f, r in zip(files, rs):
            m = re.search(r"LIFE_DRIFT_FOR=(\d+)s", open(f).read())
            secs = float(m.group(1)) if m else 300.0
            acks.append(r["acks"] / secs)
        link = ""
        if col("up"):
            link = cell(col("up"), "{:.2f}") + " / " + cell(col("down"), "{:.2f}")
        print(
            f"| {os.path.basename(p)} | {len(rs)} | {cell(col('lag'), '{:.2f}')} | {cell(col('lagmax'), '{:.2f}')} | "
            f"{cell(col('end'), '{:.1f}')} | {cell(col('applied'), '{:.2f}')} | {cell(col('busy'), '{:.0f}')}% | "
            f"{cell(col('site'), '{:.1f}')}% | {cell(col('caddy'), '{:.2f}')}% | {cell(col('kbs'), '{:.2f}')} | "
            f"{cell(acks, '{:.2f}')} | {link} | {cell(col('load'), '{:.1f}')} |"
        )


main(sys.argv[1:])
