#!/usr/bin/env bash
# RUNS ON vm3 / vm4. Where does the CPU go? For DURATION seconds, every INTERVAL seconds, tagged lines on stdout:
#   STAT|epoch|user|nice|system|idle|iowait|irq|softirq|steal     /proc/stat, cumulative jiffies of all CPUs
#   SIRQ|epoch|NET_RX|NET_TX|TIMER|SCHED|RCU                      /proc/softirqs, cumulative counts summed over CPUs
#   PSI|epoch|some_avg10|some_avg60                               /proc/pressure/cpu (share of time that tasks waited for a CPU)
#   PS|<pidstat -h -H line>: epoch uid pid %usr %system %guest %wait %CPU cpu cswch/s nvcswch/s command   (every process; %wait = waiting for a CPU,
#                                                                 nvcswch/s = involuntary switches: the process was pre-empted)
#   PIDS|pid|container                                            which container each process belongs to (printed once, after 10 s)
# usage: node-prof.sh <seconds> [interval]
DUR=$1; INTERVAL=${2:-5}
COUNT=$(( DUR / INTERVAL ))
LC_ALL=C pidstat -h -H -u -w "$INTERVAL" "$COUNT" 2>/dev/null | awk '/^[0-9]/{print "PS|" $0; fflush()}' &
END=$(( $(date +%s) + DUR ))
sleep 10
# container of each process, from its cgroup (docker top missed some, e.g. the Kafka JVM)
docker ps --no-trunc --format '{{.ID}} {{.Names}}' > /tmp/.nodeprof.ids
for d in /proc/[0-9]*; do
	id=$(grep -o 'docker-[0-9a-f]*' "$d/cgroup" 2>/dev/null | head -1 | cut -c8-)
	[ -n "$id" ] && awk -v id="$id" -v pid="${d#/proc/}" '$1==id{n=$2; sub(/^digiwal-dev-/,"",n); print "PIDS|" pid "|" n}' /tmp/.nodeprof.ids
done
while [ "$(date +%s)" -lt "$END" ]; do
	t=$(date +%s)
	awk -v t="$t" '/^cpu /{print "STAT|" t "|" $2 "|" $3 "|" $4 "|" $5 "|" $6 "|" $7 "|" $8 "|" $9}' /proc/stat
	awk -v t="$t" '$1=="NET_RX:"||$1=="NET_TX:"||$1=="TIMER:"||$1=="SCHED:"||$1=="RCU:"{s=0; for(i=2;i<=NF;i++) s+=$i; v[$1]=s} END{print "SIRQ|" t "|" v["NET_RX:"] "|" v["NET_TX:"] "|" v["TIMER:"] "|" v["SCHED:"] "|" v["RCU:"]}' /proc/softirqs
	awk -v t="$t" '/^some/{split($2,a,"="); split($3,b,"="); print "PSI|" t "|" a[2] "|" b[2]}' /proc/pressure/cpu
	sleep "$INTERVAL"
done
wait
