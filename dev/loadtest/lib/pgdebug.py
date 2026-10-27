#!/usr/bin/env python3
"""Summary of a 50-debug-pg.sh run. usage: pgdebug.py <outdir> <step_s>
Reads <outdir>/samples.vm4.txt (lib/pgdebug-sampler.sh), metrics.{before,after}.<container>.txt and k6.log (SETUP_DONE = when the load starts)."""
import collections, glob, os, re, sys
out, step_s = sys.argv[1], int(sys.argv[2])
t0 = int(re.search(r"SETUP_DONE (\d+)", open(f"{out}/k6.log").read()).group(1)) // 1000
lo, hi = t0 + 3, t0 + step_s            # the load window, minus the ramp-up seconds

rows = collections.defaultdict(list)
for l in open(f"{out}/samples.vm4.txt"):
    p = l.rstrip("\n").split("|")
    if len(p) >= 6 and p[1].isdigit() and lo <= int(p[1]) <= hi:
        rows[p[0]].append(p)

def avg(xs): return sum(xs) / len(xs) if xs else 0.0

print(f"== Postgres connections to the ledger database during the load ({len(rows['CONN'])} samples, one per ~2 s)")
if rows["CONN"]:
    cols = {"connections": 2, "active": 3, "idle": 4, "idle in transaction": 5, "active and waiting": 6}
    for name, i in cols.items():
        v = [int(r[i]) for r in rows["CONN"]]
        print(f"  {name:20} avg {avg(v):6.1f}   max {max(v):4d}")
    print(f"  oldest open transaction: max {max(float(r[7]) for r in rows['CONN']):.3f} s")
    print("  time series (every ~10 s):  t+s  connections  active  idle  idle_in_tx  waiting  oldest_xact_s")
    for r in rows["CONN"][::5]:
        print(f"      {int(r[1]) - t0:4d}   {r[2]:>5}   {r[3]:>5}   {r[4]:>5}   {r[5]:>5}   {r[6]:>5}   {r[7]:>8}")
    print("  who holds them (avg connections / avg active):")
    by = collections.defaultdict(list)
    for r in rows["CLIENT"]: by[r[7]].append((int(r[2]), int(r[3])))
    n = len(rows["CONN"]) or 1
    for ip, v in sorted(by.items()): print(f"      {ip:16} {sum(a for a, _ in v) / n:6.1f} / {sum(b for _, b in v) / n:5.1f}")
    print("  what the active backends wait for (share of all active-backend samples):")
    w = collections.Counter()
    for r in rows["WAIT"]: w[r[7]] += int(r[2])
    tot = sum(w.values()) or 1
    for k, v in w.most_common(6): print(f"      {k:32} {100 * v / tot:5.1f}%")
else:
    print("  no samples inside the load window: check samples.vm4.txt")

print("\n== the ledger-services' pools (gauges, sampled)")
by = collections.defaultdict(list)
for r in rows["POOL"]:
    try: by[r[2]].append((int(r[3]), int(r[4]), int(r[5])))
    except ValueError: pass
for c, v in sorted(by.items()):
    print(f"  {c:20} acquired avg {avg([a for a, _, _ in v]):5.1f} max {max(a for a, _, _ in v):3d} | idle avg {avg([i for _, i, _ in v]):5.1f} | max_connections {v[0][2]}")

def counters(path):
    d = collections.defaultdict(float)
    if not os.path.exists(path): return d
    for l in open(path):
        m = re.match(r'(digiwal_\w+)(\{.*\})? ([0-9.e+-]+)$', l)   # a label value may contain braces: route="GET /v1/ledger/balances/{user_id}"
        if not m: continue
        name, lab = m.group(1), m.group(2) or ""
        if name.startswith("digiwal_http_request_duration_seconds_") and re.search(r'route="GET /v1/(ledger/)?balances', lab):   # the gateway's route and the ledger-service's
            d[name.rsplit("_", 1)[1] + "@http"] += float(m.group(3))
        elif name in ("digiwal_db_pool_acquires_total", "digiwal_db_pool_empty_acquires_total", "digiwal_db_pool_acquire_seconds_total"):
            d[name] += float(m.group(3))
    return d

def delta(prefix, names):
    tot = collections.defaultdict(float)
    for f in glob.glob(f"{out}/metrics.after.{prefix}*.txt"):
        b = counters(f.replace("metrics.after", "metrics.before")); a = counters(f)
        for k in set(a) | set(b): tot[k] += max(a[k] - b[k], 0.0)
    return tot

print("\n== how long each hop takes (averages over the whole run; GET /v1/balances)")
gw, lg = delta("api-gateway", None), delta("ledger-service", None)
g = 1000 * gw["sum@http"] / gw["count@http"] if gw["count@http"] else 0
l = 1000 * lg["sum@http"] / lg["count@http"] if lg["count@http"] else 0
acq = lg["digiwal_db_pool_acquires_total"]
wait = 1000 * lg["digiwal_db_pool_acquire_seconds_total"] / acq if acq else 0
print(f"  api-gateway request            : {g:7.1f} ms  ({int(gw['count@http'])} requests)")
print(f"  ledger-service request         : {l:7.1f} ms  ({int(lg['count@http'])} requests)")
print(f"  gateway minus ledger           : {g - l:7.1f} ms  (balancer, TLS, WireGuard, the gateway's own work, API-key lookup)")
print(f"  ledger waits for a PG connection: {wait:6.1f} ms per acquire  ({int(lg['digiwal_db_pool_empty_acquires_total'])} of {int(acq)} acquires had to wait = {100 * lg['digiwal_db_pool_empty_acquires_total'] / acq if acq else 0:.1f}%)")
print("  time inside Postgres per statement: see statements.txt (mean / max exec ms)")


# ---- histograms added on 2026-10-27: what each side measures itself (client-side calls, pool, statements). Delta of the buckets between before and after.
def hist(prefix, name, labels=""):
    """Bucket counts of one histogram summed over the containers of a service: {le: count}, sum, count -- after minus before."""
    def read(path):
        b, sm, ct = collections.defaultdict(float), 0.0, 0.0
        if not os.path.exists(path): return b, sm, ct
        for l in open(path):
            if not l.startswith(name + "_") or labels not in l: continue
            m = re.match(name + r'_(bucket|sum|count)(\{.*\}) ([0-9.e+-]+)$', l)
            if not m: continue
            if m.group(1) == "bucket":
                b[float(re.search(r'le="([^"]+)"', m.group(2)).group(1).replace("+Inf", "inf"))] += float(m.group(3))
            elif m.group(1) == "sum": sm += float(m.group(3))
            else: ct += float(m.group(3))
        return b, sm, ct
    B, S, C = collections.defaultdict(float), 0.0, 0.0
    for f in glob.glob(f"{out}/metrics.after.{prefix}*.txt"):
        ab, asum, acnt = read(f); bb, bsum, bcnt = read(f.replace("metrics.after", "metrics.before"))
        for k in ab: B[k] += max(ab[k] - bb[k], 0.0)
        S += max(asum - bsum, 0.0); C += max(acnt - bcnt, 0.0)
    return B, S, C

def quantile(B, C, q):
    """Upper edge of the bucket holding the q-quantile (so: at most this value; exact to the bucket width)."""
    if not C: return float("nan")
    for le in sorted(B):
        if B[le] >= q * C: return le
    return float("inf")

def line(title, prefix, name, labels=""):
    B, S, C = hist(prefix, name, labels)
    if not C:
        print(f"  {title:46} no data"); return
    print(f"  {title:46} n={int(C):7d}  avg {1000 * S / C:7.1f}  p50 <= {1000 * quantile(B, C, .5):7.1f}  p95 <= {1000 * quantile(B, C, .95):7.1f}  p99 <= {1000 * quantile(B, C, .99):7.1f} ms")

print("\n== measured by the services themselves (histograms; avg and the upper edge of the bucket that holds each percentile)")
line("gateway: whole request GET /v1/balances", "api-gateway", "digiwal_http_request_duration_seconds", 'route="GET /v1/balances"')
line("gateway: call to ledger, until response headers", "api-gateway", "digiwal_upstream_request_duration_seconds", 'kind="ledger"')
line("ledger: handler GET /v1/ledger/balances/{user_id}", "ledger-service", "digiwal_http_request_duration_seconds", 'route="GET /v1/ledger/balances')
line("ledger: wait for a connection from the pool", "ledger-service", "digiwal_db_pool_acquire_wait_seconds")
line("ledger: a connection stays checked out", "ledger-service", "digiwal_db_pool_hold_seconds")
line("ledger: one statement (start to last row)", "ledger-service", "digiwal_db_query_duration_seconds")
print("  new connections gateway -> ledger:")
line("    TCP connect", "api-gateway", "digiwal_upstream_setup_seconds", 'stage="connect"')
line("    TLS handshake", "api-gateway", "digiwal_upstream_setup_seconds", 'stage="tls"')
new = reused = 0.0
for f in glob.glob(f"{out}/metrics.after.api-gateway*.txt"):
    for ff, sign in ((f, 1), (f.replace("metrics.after", "metrics.before"), -1)):
        if not os.path.exists(ff): continue
        for l in open(ff):
            m = re.match(r'digiwal_upstream_connections_total\{kind="ledger",result="(new|reused)".*\} ([0-9.e+-]+)$', l)
            if m:
                if m.group(1) == "new": new += sign * float(m.group(2))
                else: reused += sign * float(m.group(2))
if new + reused: print(f"  calls to ledger on a new connection: {int(new)} of {int(new + reused)} ({100 * new / (new + reused):.1f}%)")
