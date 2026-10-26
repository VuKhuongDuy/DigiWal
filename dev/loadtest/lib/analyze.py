#!/usr/bin/env python3
"""Per-step table for one k6 run: achieved rate, errors, latency, and what the Postgres containers did.
usage: analyze.py <outdir> <steps> <step_s> <gap_s>"""
import json, re, sys

out, steps, step_s, gap_s = sys.argv[1], [int(x) for x in sys.argv[2].split(",")], int(sys.argv[3]), int(sys.argv[4])
summary = json.load(open(f"{out}/summary.json"))["metrics"]
m = re.search(r"SETUP_DONE (\d+) users=(\d+) of (\d+)", open(f"{out}/k6.log").read())
if not m:
    sys.exit("k6.log has no SETUP_DONE line: the run did not start")
t0, users = int(m.group(1)) / 1000.0, m.group(2)


def load(name):
    rows = []
    try:
        for line in open(f"{out}/{name}"):
            p = line.split()
            if len(p) == 7 or len(p) == 6:
                rows.append([int(x) for x in p])
    except OSError:
        pass
    return rows


def window(rows, a, b):
    r = [x for x in rows if a <= x[0] <= b]
    if len(r) < 2:
        return None
    first, last = r[0], r[-1]
    dt = max(last[0] - first[0], 1)
    cores = (last[1] - first[1]) / 1e6 / dt
    thr = 100.0 * (last[3] - first[3]) / (last[2] - first[2]) if last[2] > first[2] else 0.0
    active = sum(x[5] for x in r) / len(r)
    backlog = r[-1][6] if len(r[-1]) > 6 else 0
    return cores, thr, active, backlog


samples = {k: load(f"sample.{k}.txt") for k in ("ledger", "gateway", "core")}


def ms(metric, key):
    return f"{summary[metric][key]:.0f}" if metric in summary else "-"


lines = [f"users with an active whitelist: {users} of {m.group(3)}",
         "step | target | achieved | dropped | failed% | write p50/p95/p99 ms | read p50/p95/p99 ms | PG ledger (vm2) cores/thr%/act | PG gateway (vm4) | PG core (vm3) | backlog"]
for i, rate in enumerate(steps):
    s = f"r{rate}"
    a = t0 + i * (step_s + gap_s)
    cnt = summary.get(f"http_reqs{{step:{s}}}", {}).get("count", 0)
    dropped = summary.get(f"dropped_iterations{{scenario:{s}}}", {}).get("count", 0)
    failed = 100 * summary.get(f"http_req_failed{{step:{s}}}", {}).get("value", 0)
    wd = "/".join(ms(f"http_req_duration{{step:{s},kind:wd}}", k) for k in ("p(50)", "p(95)", "p(99)"))
    rd = "/".join(ms(f"http_req_duration{{step:{s},kind:read}}", k) for k in ("p(50)", "p(95)", "p(99)"))
    cols, backlog = [], "-"
    for k in ("ledger", "gateway", "core"):
        w = window(samples[k], a + 5, a + step_s)
        cols.append(f"{w[0]:.2f}/{w[1]:.0f}%/{w[2]:.1f}" if w else "n/a")
        if k == "core" and w:
            backlog = str(w[3])
    lines.append(f"{s} | {rate} | {cnt / step_s:.0f} | {int(dropped)} | {failed:.2f} | {wd} | {rd} | {cols[0]} | {cols[1]} | {cols[2]} | {backlog}")
print("\n".join(lines))
