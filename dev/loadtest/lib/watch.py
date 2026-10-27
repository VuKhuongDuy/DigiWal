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
for c in $(docker ps --format '{{.Names}}' | grep -E '^digiwal-dev-(api-gateway|ledger-service)'); do
  s=api-gateway; case $c in *ledger-service*) s=ledger-service;; esac
  docker exec $c wget -qO- http://127.0.0.1:9100/metrics 2>/dev/null | grep -E '^digiwal_(db_pool_(acquire_seconds_total|acquires_total|empty_acquires_total)|cache_requests_total|http_request_duration_seconds_(sum|count))' | sed "s/^/M $s /"
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
            if m:
                k = (p[1], m.group(1), m.group(2) or ""); d["m"][k] = d["m"].get(k, 0.0) + float(m.group(3))   # two containers of a service: add up
    return d

def busy(a, b):
    if not a["stat"] or not b["stat"]: return None
    d = [y - x for x, y in zip(a["stat"], b["stat"])]; tot = sum(d) or 1
    return 100 * (1 - d[2] / tot), 100 * d[1] / tot

def cores(a, b, name):
    if name not in a["cg"] or name not in b["cg"]: return 0.0
    return (b["cg"][name] - a["cg"][name]) / 1e6 / max(b["t"] - a["t"], 1)

def cores_of(a, b, prefix):
    """Cores of every container whose name starts with prefix (api-gateway-1, api-gateway-2, ledger-service-1, ledger-service-2-1, ...)."""
    return sum(cores(a, b, n) for n in b["cg"] if n.startswith(prefix))

def delta(a, b, svc, metric, label_re=None):
    tot = 0.0
    for (s, m, lab), v in b["m"].items():
        if s == svc and m == metric and (label_re is None or re.search(label_re, lab)):
            tot += v - a["m"].get((s, m, lab), 0.0)
    return tot

def delta_all(prev, cur, nodes, svc, metric, label_re=None):
    """The same counter summed over the nodes that run the service (a node that restarted contributes nothing for that interval)."""
    return sum(max(delta(prev[n], cur[n], svc, metric, label_re), 0.0) for n in nodes)

nodes = ("vm2", "vm3", "vm4")
prev = {n: snap(n) for n in nodes}
print("time     k6 | busy% (sys%) vm2 vm3 vm4 | cores (both nodes summed): gw  ledger redis pg-gw pg-ledger | pool wait ms: gw ledger | cache hit%: apikey bal (err) | gateway: req/s avg ms")
end = time.time() + TOTAL
while time.time() < end:
    time.sleep(INTERVAL)
    cur = {n: snap(n) for n in nodes}
    b = {n: busy(prev[n], cur[n]) for n in nodes}
    fmt = lambda x: "  - " if x is None else f"{x[0]:3.0f}({x[1]:2.0f})"
    c = lambda node, name: cores(prev[node], cur[node], name)
    gws = ("vm3", "vm4")                      # the two gateways are on vm3, the two ledger-services on vm4
    def wait(svc):
        n = delta_all(prev, cur, gws, svc, "digiwal_db_pool_acquires_total")
        return 1000 * delta_all(prev, cur, gws, svc, "digiwal_db_pool_acquire_seconds_total") / n if n > 0 else 0.0
    def hit(svc, comp):
        f = lambda r: delta_all(prev, cur, gws, svc, "digiwal_cache_requests_total", f'component="{comp}".*result="{r}"|result="{r}".*component="{comp}"')
        h, m, e = f("hit"), f("miss"), f("error")
        return (100 * h / (h + m + e) if h + m + e > 0 else 0.0), e
    reqs = delta_all(prev, cur, gws, "api-gateway", "digiwal_http_request_duration_seconds_count", 'route="GET /v1/balances"')
    secs = delta_all(prev, cur, gws, "api-gateway", "digiwal_http_request_duration_seconds_sum", 'route="GET /v1/balances"')
    dt = max(cur["vm3"]["t"] - prev["vm3"]["t"], 1)
    ah, _ = hit("api-gateway", "apikey"); bh, be = hit("ledger-service", "balances")
    k6 = "RUN" if cur["vm2"]["k6"] else "idle"
    print(f"{time.strftime('%H:%M:%S')} {k6:4} | {fmt(b['vm2'])} {fmt(b['vm3'])} {fmt(b['vm4'])} | {cores_of(prev['vm3'], cur['vm3'], 'api-gateway'):4.2f} {cores_of(prev['vm4'], cur['vm4'], 'ledger-service'):5.2f} {c('vm3','redis-1')+c('vm4','redis-1'):5.2f} {c('vm3','postgres-gateway-1'):5.2f} {c('vm4','postgres-1'):7.2f} | {wait('api-gateway'):6.1f} {wait('ledger-service'):6.1f} | {ah:5.1f} {bh:5.1f} ({be:.0f}) | {reqs/dt:6.0f} {1000*secs/reqs if reqs>0 else 0:6.0f}", flush=True)
    prev = cur
