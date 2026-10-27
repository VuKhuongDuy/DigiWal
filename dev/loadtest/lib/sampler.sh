#!/usr/bin/env bash
# Samples, every INTERVAL seconds for DURATION seconds, one Postgres container on this node, from its cgroup (no Prometheus):
#   epoch  cpu_usage_usec  nr_periods  nr_throttled  throttled_usec  active_backends  pipeline_backlog
# usage: sampler.sh <seconds> <container> [db-for-backlog]    (backlog = withdrawals reserved/validated, only if a db is given)
DUR=$1; C=$2; DB=${3:-}; INTERVAL=${INTERVAL:-5}
ID=$(docker inspect -f '{{.Id}}' "$C"); F=/sys/fs/cgroup/system.slice/docker-$ID.scope/cpu.stat
END=$(( $(date +%s) + DUR ))
while [ "$(date +%s)" -lt "$END" ]; do
	U=$(awk '/^usage_usec/{u=$2} /^nr_periods/{p=$2} /^nr_throttled/{t=$2} /^throttled_usec/{tu=$2} END{print u,p,t,tu}' "$F")
	A=$(docker exec "$C" psql -U custody -d postgres -tAc "select count(*) from pg_stat_activity where state='active' and pid<>pg_backend_pid()" 2>/dev/null || echo 0)
	B=0
	[ -n "$DB" ] && B=$(docker exec "$C" psql -U custody -d "$DB" -tAc "select count(*) from withdrawal_requests where status in ('reserved','validated')" 2>/dev/null || echo 0)
	echo "$(date +%s) $U ${A:-0} ${B:-0}"
	sleep "$INTERVAL"
done
