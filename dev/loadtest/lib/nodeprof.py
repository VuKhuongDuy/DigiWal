#!/usr/bin/env python3
"""CPU breakdown per node during the load. usage: nodeprof.py <outdir> <step_s>   (reads nodeprof.vm3.txt, nodeprof.vm4.txt, k6.log)"""
import collections, os, re, sys
out, step_s = sys.argv[1], int(sys.argv[2])
t0 = int(re.search(r"SETUP_DONE (\d+)", open(f"{out}/k6.log").read()).group(1)) // 1000
lo, hi = t0 + 3, t0 + step_s

def node(name):
    path = f"{out}/nodeprof.{name}.txt"
    if not os.path.exists(path): print(f"== {name}: no data"); return
    stat, sirq, psi, pids, ps = [], [], [], {}, collections.defaultdict(list)
    for l in open(path):
        p = l.rstrip("\n").split("|")
        try:
            if p[0] == "STAT": stat.append((int(p[1]), [int(x) for x in p[2:10]]))
            elif p[0] == "SIRQ": sirq.append((int(p[1]), [int(x or 0) for x in p[2:7]]))
            elif p[0] == "PSI": psi.append((int(p[1]), float(p[2])))
            elif p[0] == "PIDS": pids[p[1]] = p[2]
            elif p[0] == "PS":
                f = p[1].split(None, 11)
                if len(f) >= 12: ps[int(f[0])].append((f[2], [float(x) for x in f[3:11]], f[11]))
        except (ValueError, IndexError): pass
    w = [s for s in stat if lo <= s[0] <= hi]
    print(f"== {name}")
    if len(w) >= 2:
        d = [b - a for a, b in zip(w[0][1], w[-1][1])]; tot = sum(d) or 1
        names = ["user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"]
        print("  CPU, all 4 cores, load window: " + "  ".join(f"{n} {100 * v / tot:.0f}%" for n, v in zip(names, d) if n != "nice"))
    ws = [s for s in sirq if lo <= s[0] <= hi]
    if len(ws) >= 2:
        dt = ws[-1][0] - ws[0][0] or 1
        d = [b - a for a, b in zip(ws[0][1], ws[-1][1])]
        print(f"  softirqs per second: NET_RX {d[0] / dt:,.0f}  NET_TX {d[1] / dt:,.0f}  TIMER {d[2] / dt:,.0f}  SCHED {d[3] / dt:,.0f}  RCU {d[4] / dt:,.0f}")
    wp = [v for t, v in psi if lo <= t <= hi]
    if wp: print(f"  CPU pressure (some, avg10): avg {sum(wp) / len(wp):.0f}%  max {max(wp):.0f}%")
    # per process group: at each sample time sum the group's members, then average over the window (columns of a pidstat line after epoch uid pid:
    # %usr %system %guest %wait %CPU cpu cswch/s nvcswch/s)
    agg = collections.defaultdict(lambda: collections.defaultdict(lambda: [0.0] * 5))
    for t, rr in ps.items():
        if not (lo <= t <= hi): continue
        for pid, v, cmd in rr:
            g = pids.get(pid) or ("host:" + (cmd.split()[0].split("/")[-1] if cmd else "?"))
            a = agg[g][t]
            a[0] += v[0]; a[1] += v[1]; a[2] += v[3]; a[3] += v[6]; a[4] += v[7]
    table = []
    for g, byt in agg.items():
        m = [sum(a[i] for a in byt.values()) / len(byt) for i in range(5)]
        table.append((m[0] + m[1], g, m))
    table.sort(reverse=True)
    print("  busiest processes (avg over the window; %CPU is of ONE core, so 100 = one full core):")
    print(f"      {'container / process':30} {'%usr':>6} {'%sys':>6} {'%wait for CPU':>14} {'voluntary sw/s':>15} {'pre-empted sw/s':>16}")
    for _, g, m in table[:10]:
        print(f"      {g:30} {m[0]:6.0f} {m[1]:6.0f} {m[2]:14.0f} {m[3]:15.0f} {m[4]:16.0f}")
    print()

for n in ("vm3", "vm4"): node(n)
