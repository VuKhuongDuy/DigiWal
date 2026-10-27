#!/usr/bin/env bash
# On a node: every 5 s one line "epoch cpu_user cpu_system cpu_idle cpu_iowait (jiffies) name=usec name=usec ..." for the digiwal containers.
DUR=$1; END=$(( $(date +%s) + DUR ))
while [ "$(date +%s)" -lt "$END" ]; do
	line="$(date +%s) $(awk '/^cpu /{print $2+$3, $4, $5, $6}' /proc/stat)"
	for c in $(docker ps --format '{{.Names}}' | grep digiwal-dev); do
		id=$(docker inspect -f '{{.Id}}' "$c" 2>/dev/null) || continue
		u=$(awk '/^usage_usec/{print $2}' /sys/fs/cgroup/system.slice/docker-$id.scope/cpu.stat 2>/dev/null) || continue
		line="$line ${c#digiwal-dev-}=$u"
	done
	echo "$line"; sleep 5
done
