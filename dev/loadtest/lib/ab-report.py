#!/usr/bin/env python3
"""Table of medians (and min-max) per variant from the JSON lines of 60-ab.sh. usage: ab-report.py <runs.jsonl>"""
import collections, json, statistics, sys
runs = collections.defaultdict(list)
for l in open(sys.argv[1]):
    if l.strip():
        r = json.loads(l); runs[r["variant"]].append(r)
cols = [("achieved", "achieved req/s", 0), ("dropped", "dropped", 0), ("failed_pct", "failed %", 2), ("k6_p50", "k6 p50 ms", 0), ("k6_p95", "k6 p95 ms", 0),
        ("k6_p99", "k6 p99 ms", 0), ("gateway_avg", "gateway avg ms", 0), ("call_avg", "call ledger avg ms", 0), ("ledger_avg", "ledger avg ms", 0),
        ("psi_vm3", "PSI vm3 %", 0), ("psi_vm4", "PSI vm4 %", 0), ("busy_vm3", "busy vm3 %", 0), ("busy_vm4", "busy vm4 %", 0), ("ledger_share_max_pct", "busiest ledger %", 0)]
print("median (min - max) over the runs of each variant:\n")
vs = sorted(runs)
w = max(len(c[1]) for c in cols)
print(f"{'':{w}}  " + "  ".join(f"{v + ' (n=' + str(len(runs[v])) + ')':>26}" for v in vs))
for key, label, dec in cols:
    cells = []
    for v in vs:
        xs = [r[key] for r in runs[v] if key in r]
        cells.append(f"{statistics.median(xs):.{dec}f} ({min(xs):.{dec}f}-{max(xs):.{dec}f})" if xs else "-")
    print(f"{label:{w}}  " + "  ".join(f"{c:>26}" for c in cells))
print("\nruns, in the order they ran:")
for v in vs:
    for r in runs[v]: print(f"  {v:8} {r['dir']}  gateway avg {r.get('gateway_avg', float('nan')):.0f} ms  achieved {r.get('achieved', float('nan')):.0f}")
