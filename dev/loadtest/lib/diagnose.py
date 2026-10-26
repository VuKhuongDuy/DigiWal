#!/usr/bin/env python3
"""Where did the time go? usage: diagnose.py <outdir> <window_s> <metrics_before_dir> <metrics_after_dir>"""
import json, os, re, sys
out, win, mb, ma = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
t0 = int(re.search(r"SETUP_DONE (\d+)", open(f"{out}/k6.log").read()).group(1)) / 1000.0

def window(node):
    rows = []
    for l in open(f"{out}/nodecpu.{node}.txt"):
        p = l.split()
        if len(p) < 6: continue
        rows.append((int(p[0]), [int(x) for x in p[1:5]], {kv.split("=")[0]: int(kv.split("=")[1]) for kv in p[5:]}))
    r = [x for x in rows if t0 + 3 <= x[0] <= t0 + win]
    return (r[0], r[-1]) if len(r) >= 2 else None

print("== CPU in the load window (4 vCPU per node)")
for node in ("vm2", "vm3", "vm4"):
    w = window(node)
    if not w: print(f"  {node}: no samples"); continue
    (ta, ha, ca), (tb, hb, cb) = w
    d = [b - a for a, b in zip(ha, hb)]; tot = sum(d) or 1
    print(f"  {node}: busy {100 * (1 - d[2] / tot):.0f}%  (user+nice {100 * d[0] / tot:.0f}%  system {100 * d[1] / tot:.0f}%  iowait {100 * d[3] / tot:.0f}%)")
    top = sorted(((k, (cb[k] - ca.get(k, 0)) / 1e6 / (tb - ta)) for k in cb if k in ca), key=lambda x: -x[1])[:4]
    print("         " + "  ".join(f"{k}={v:.2f}" for k, v in top))

def parse(path):
    sums, cnts, pool = {}, {}, {}
    for l in open(path):
        m = re.match(r'digiwal_http_request_duration_seconds_(sum|count)\{([^}]*)\} ([0-9.e+-]+)', l)
        if m:
            lab = dict(re.findall(r'(\w+)="([^"]*)"', m.group(2)))
            (sums if m.group(1) == "sum" else cnts)[(lab.get("route"), lab.get("code"))] = float(m.group(3))
        m = re.match(r'digiwal_db_pool_(acquire_seconds_total|acquires_total|empty_acquires_total)(\{[^}]*\})? ([0-9.e+-]+)', l)
        if m: pool[m.group(1)] = float(m.group(3))
    return sums, cnts, pool

print("== time per request at each hop (averages over the run; the gap between hops is network, TLS and queueing)")
for svc in ("api-gateway", "ledger-service"):
    try:
        sa, ca, pa = parse(f"{mb}/{svc}.txt"); sb, cb, pb = parse(f"{ma}/{svc}.txt")
    except OSError:
        continue
    rows = []
    for k in cb:
        dn = cb[k] - ca.get(k, 0)
        if dn >= 20: rows.append((k, dn, 1000 * (sb[k] - sa.get(k, 0)) / dn))
    for k, n, ms in sorted(rows, key=lambda r: -r[1])[:3]:
        print(f"  {svc:15} {k[0]} code={k[1]}: {int(n)} requests, avg {ms:.0f} ms")
    acq = pb.get("acquires_total", 0) - pa.get("acquires_total", 0)
    if acq > 0:
        wait = 1000 * (pb.get("acquire_seconds_total", 0) - pa.get("acquire_seconds_total", 0)) / acq
        waited = 100 * (pb.get("empty_acquires_total", 0) - pa.get("empty_acquires_total", 0)) / acq
        print(f"  {svc:15} DB pool: avg wait for a connection {wait:.1f} ms, {waited:.0f}% of acquires had to wait")
