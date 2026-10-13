"""Medians and ranges per configuration from a suite's JSON lines."""
import json
import statistics
import sys
from collections import defaultdict

groups = defaultdict(list)
meta = []
for line in open(sys.argv[1]):
    d = json.loads(line)
    if "result" not in d:
        meta.append(d)
        continue
    r = d["result"]
    if r is None:
        print("FAILED:", d["cmd"])
        continue
    r["_load"] = d["load"]
    groups[(r["Bench"], r["Store"], r["Mode"], r["Producers"])].append(r)


def fmt(values, digits=1):
    values = [v for v in values if v is not None]
    if not values:
        return "-"
    m = statistics.median(values)
    return f"{m:,.{digits}f} ({min(values):,.{digits}f} to {max(values):,.{digits}f})"


for m in meta:
    print(m)
for key in sorted(groups):
    rs = groups[key]
    bench, store, mode, producers = key
    head = f"{bench:8} {store:8} {mode:32} p={producers:<3} n={len(rs)}"
    if bench == "write":
        ex = lambda k: [r.get("Extra", {}).get(k) for r in rs]
        print(head, "cmds/s", fmt([r["CmdsPerSec"] for r in rs], 0), "| cmds/tx", fmt([r["CmdsPerTx"] for r in rs]),
              "| p50 ms", fmt([r["P50ms"] for r in rs], 2), "| p99 ms", fmt([r["P99ms"] for r in rs], 2),
              "| refused%", fmt([100 * r["Refused"] / max(r["Commands"], 1) for r in rs], 2),
              "| retried/cmd", fmt([r.get("Retried", 0) / max(r["Commands"], 1) for r in rs], 2),
              "| conflicts", sum(r.get("Conflicts", 0) for r in rs),
              "| syncs/tx", fmt(ex("syncs_per_tx"), 3), "| ms/sync", fmt(ex("sync_ms_per_sync"), 2),
              "| commit ms/batch", fmt(ex("commit_ms_per_batch"), 2), "| read ms/batch", fmt(ex("read_ms_per_batch"), 2),
              "| threads", fmt(ex("threads"), 0))
    else:
        print(head, "p50 ms", fmt([r["P50ms"] for r in rs], 3), "| p99 ms", fmt([r["P99ms"] for r in rs], 3))
        keys = sorted({k for r in rs for k in r.get("Extra", {})})
        for k in keys:
            print("    ", k, fmt([r["Extra"].get(k) for r in rs], 3))
loads = sorted({r["_load"] for rs in groups.values() for r in rs})
print("load averages seen (1, 5, 15 min):", loads[0], "...", loads[-1], "of", len(loads))
