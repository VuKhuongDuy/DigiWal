#!/usr/bin/env python3
"""Live view while a load test runs (read-only). One line every INTERVAL seconds:
node CPU busy% (vm2 vm3 vm4), cores of the containers that matter, DB pool waits, cache hit ratios, gateway req/s and avg latency.
usage: watch.py [seconds] [interval]   run from vm2"""
import re, subprocess, sys, time

TOTAL = int(sys.argv[1]) if len(sys.argv) > 1 else 600
INTERVAL = int(sys.argv[2]) if len(sys.argv) > 2 else 15

SNAP = r'''
awk '/^cpu /{print "STAT", $2+$3, $4, $5, $6}' /proc/stat
for c in $(docker ps --format '{{.Names}}' | grep -E 'api-gateway|ledger-service|redis|postgres|withdrawal|kafka'); do
  id=$(docker inspect -f '{{.Id}}' $c 2>/dev/null) || continue
  echo "CG ${c#digiwal-dev-} $(awk '/^usage_usec/{print $2}' /sys/fs/cgroup/system.slice/docker-$id.scope/cpu.stat 2>/dev/null)"
done
for s in api-gateway ledger-service; do
  docker exec digiwal-dev-$s-1 wget -qO- http://127.0.0.1:9100/metrics 2>/dev/null | grep -E '^digiwal_(db_pool_(acquire_seconds_total|acquires_total|empty_acquires_total)|cache_requests_total|http_request_duration_seconds_(sum|count))' | sed "s/^/M $s /"
done
echo "K6 $(docker ps --format '{{.Image}}' | grep -c k6)"
'''

def snap(node):
    cmd = ["bash", "-c", SNAP] if node == "vm2" else ["ssh", node, SNAP]
    t = time.time()
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=40).stdout
    d = {"t": t, "stat": None, "cg": {}, "m": {}, "k6": 0}
    for l in out.splitlines():
        p = l.split()
        if not p: continue
        if p[0] == "STAT": d["stat"] = [int(x) for x in p[1:5]]
        elif p[0] == "CG" and len(p) == 3: d["cg"][p[1]] = int(p[2])
        elif p[0] == "K6": d["k6"] = int(p[1])
        elif p[0] == "M":
            m = re.match(r'(\S+?)(\{[^}]*\})? ([0-9.e+-]+)$', l.split(" ", 2)[2])
            if m: d["m"][(p[1], m.group(1), m.group(2) or "")] = float(m.group(3))
    return d

def busy(a, b):
    if not a["stat"] or not b["stat"]: return None
    d = [y - x for x, y in zip(a["stat"], b["stat"])]; tot = sum(d) or 1
    return 100 * (1 - d[2] / tot), 100 * d[1] / tot

def cores(a, b, name):
    if name not in a["cg"] or name not in b["cg"]: return 0.0
    return (b["cg"][name] - a["cg"][name]) / 1e6 / max(b["t"] - a["t"], 1)

def delta(a, b, svc, metric, label_re=None):
    tot = 0.0
    for (s, m, lab), v in b["m"].items():
        if s == svc and m == metric and (label_re is None or re.search(label_re, lab)):
            tot += v - a["m"].get((s, m, lab), 0.0)
    return tot

nodes = ("vm2", "vm3", "vm4")
prev = {n: snap(n) for n in nodes}
print("time     k6 | busy% (sys%) vm2 vm3 vm4 | cores: gw  ledger redis pg-gw pg-ledger | pool wait ms: gw ledger | cache hit%: apikey bal (err) | gateway: req/s avg ms")
end = time.time() + TOTAL
while time.time() < end:
    time.sleep(INTERVAL)
    cur = {n: snap(n) for n in nodes}
    b = {n: busy(prev[n], cur[n]) for n in nodes}
    fmt = lambda x: "  - " if x is None else f"{x[0]:3.0f}({x[1]:2.0f})"
    gw, led = ("vm3", "api-gateway-1"), ("vm4", "ledger-service-1")
    c = lambda node, name: cores(prev[node], cur[node], name)
    def wait(svc, node):
        n = delta(prev[node], cur[node], svc, "digiwal_db_pool_acquires_total")
        return 1000 * delta(prev[node], cur[node], svc, "digiwal_db_pool_acquire_seconds_total") / n if n > 0 else 0.0
    def hit(svc, node, comp):
        h = delta(prev[node], cur[node], svc, "digiwal_cache_requests_total", f'component="{comp}".*result="hit"|result="hit".*component="{comp}"')
        m = delta(prev[node], cur[node], svc, "digiwal_cache_requests_total", f'component="{comp}".*result="miss"|result="miss".*component="{comp}"')
        e = delta(prev[node], cur[node], svc, "digiwal_cache_requests_total", f'component="{comp}".*result="error"|result="error".*component="{comp}"')
        return (100 * h / (h + m + e) if h + m + e > 0 else 0.0), e
    reqs = delta(prev["vm3"], cur["vm3"], "api-gateway", "digiwal_http_request_duration_seconds_count", 'route="GET /v1/balances"')
    secs = delta(prev["vm3"], cur["vm3"], "api-gateway", "digiwal_http_request_duration_seconds_sum", 'route="GET /v1/balances"')
    dt = max(cur["vm3"]["t"] - prev["vm3"]["t"], 1)
    ah, _ = hit("api-gateway", "vm3", "apikey"); bh, be = hit("ledger-service", "vm4", "balances")
    k6 = "RUN" if cur["vm2"]["k6"] else "idle"
    print(f"{time.strftime('%H:%M:%S')} {k6:4} | {fmt(b['vm2'])} {fmt(b['vm3'])} {fmt(b['vm4'])} | {c('vm3','api-gateway-1'):4.2f} {c('vm4','ledger-service-1'):5.2f} {c('vm3','redis-1'):5.2f} {c('vm3','postgres-gateway-1'):5.2f} {c('vm2','postgres-1'):7.2f} | {wait('api-gateway','vm3'):6.1f} {wait('ledger-service','vm4'):6.1f} | {ah:5.1f} {bh:5.1f} ({be:.0f}) | {reqs/dt:6.0f} {1000*secs/reqs if reqs>0 else 0:6.0f}", flush=True)
    prev = cur
