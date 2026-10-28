#!/usr/bin/env python3
"""One 50-debug-pg.sh result directory -> one JSON line of numbers (used by 60-ab.sh). usage: ab-collect.py <dir>"""
import json, os, re, sys
d = sys.argv[1]
r = {"dir": os.path.basename(d.rstrip("/"))}
try:
    row = [l for l in open(f"{d}/table.txt") if re.match(r"r\d+ \|", l)][0].split("|")
    p = [x.strip() for x in row]
    r.update(achieved=float(p[2]), dropped=float(p[3]), failed_pct=float(p[4]))
    r["k6_p50"], r["k6_p95"], r["k6_p99"] = [float(x) for x in p[8].split("/")]
except Exception as e:
    r["error"] = f"table: {e}"
try:
    s = open(f"{d}/summary.txt").read()
    for key, pat in (("gateway_avg", r"gateway: whole request GET /v1/balances\s+n=\s*\d+\s+avg\s+([\d.]+)"),
                     ("call_avg", r"gateway: call to ledger, until response headers\s+n=\s*\d+\s+avg\s+([\d.]+)"),
                     ("ledger_avg", r"ledger: handler GET[^\n]*?n=\s*\d+\s+avg\s+([\d.]+)")):
        m = re.search(pat, s)
        if m: r[key] = float(m.group(1))
    for key, pat in (("pool_wait_avg", r"ledger: wait for a connection from the pool\s+n=\s*\d+\s+avg\s+([\d.]+)"),
                     ("pool_acquires", r"ledger: wait for a connection from the pool\s+n=\s*(\d+)"),
                     ("hold_avg", r"ledger: a connection stays checked out\s+n=\s*\d+\s+avg\s+([\d.]+)"),
                     ("stmt_avg", r"ledger: one statement \(start to last row\)\s+n=\s*\d+\s+avg\s+([\d.]+)")):
        m = re.search(pat, s)
        if m: r[key] = float(m.group(1))
    v4 = s.split("== vm4", 1)[1] if "== vm4" in s else ""
    cores = [(float(a) + float(b)) / 100 for a, b in re.findall(r"^\s+ledger-service-[\w-]+\s+(\d+)\s+(\d+)\s", v4, re.M)]
    if cores: r["ledger_cores"] = sum(cores)
    psi = re.findall(r"CPU pressure \(some, avg10\): avg (\d+)%", s)
    if len(psi) >= 2: r["psi_vm3"], r["psi_vm4"] = float(psi[0]), float(psi[1])
    busy = re.findall(r"CPU, all 4 cores, load window: user (\d+)%\s+system (\d+)%\s+idle (\d+)%", s)
    if len(busy) >= 2: r["busy_vm3"], r["busy_vm4"] = [100.0 - float(b[2]) for b in busy[:2]]
except Exception as e:
    r["error"] = f"summary: {e}"
cnt = []
for c in ("ledger-service-1", "ledger-service-2-1"):
    t = {}
    for ph in ("before", "after"):
        n = 0.0
        try:
            for l in open(f"{d}/metrics.{ph}.ledger-service.{c}.txt"):
                if l.startswith("digiwal_http_request_duration_seconds_count") and "balances" in l: n += float(l.rsplit(" ", 1)[1])
        except OSError: pass
        t[ph] = n
    cnt.append(t["after"] - t["before"])
if sum(cnt) > 0: r["ledger_share_max_pct"] = 100.0 * max(cnt) / sum(cnt)
print(json.dumps(r))
