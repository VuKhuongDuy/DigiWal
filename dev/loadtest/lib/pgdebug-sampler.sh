#!/usr/bin/env bash
# RUNS ON vm4. Every INTERVAL seconds for DURATION seconds, one line per fact about the ledger Postgres and the ledger-services' pools:
#   CONN|epoch|connections|active|idle|idle_in_tx|active_and_waiting|oldest_open_xact_s
#   WAIT|epoch|backends|0|0|0|0|wait_event_type:wait_event       (active backends only; "cpu:-" = running, not waiting)
#   CLIENT|epoch|connections|active|0|0|0|client_ip              (which ledger-service holds them)
#   POOL|epoch|container|acquired|idle|max                        (each ledger-service's own view of its pool)
# usage: pgdebug-sampler.sh <seconds> [interval]. The sampler connects as the superuser over the local socket (it keeps working when all the other
# slots are taken: 3 are reserved).
DUR=$1; INTERVAL=${2:-2}
END=$(( $(date +%s) + DUR ))
Q="select 'CONN', extract(epoch from now())::bigint, count(*), count(*) filter (where state='active'), count(*) filter (where state='idle'),
       count(*) filter (where state like 'idle in transaction%'), count(*) filter (where state='active' and wait_event_type is not null),
       coalesce(max(extract(epoch from now()-xact_start)) filter (where state<>'idle'),0)::numeric(9,3)::text
   from pg_stat_activity where datname='ledger'
 union all
 select 'WAIT', extract(epoch from now())::bigint, count(*), 0,0,0,0, coalesce(wait_event_type,'cpu')||':'||coalesce(wait_event,'-')
   from pg_stat_activity where datname='ledger' and state='active' group by wait_event_type, wait_event
 union all
 select 'CLIENT', extract(epoch from now())::bigint, count(*), count(*) filter (where state='active'), 0,0,0, coalesce(host(client_addr),'local')
   from pg_stat_activity where datname='ledger' group by client_addr"
while [ "$(date +%s)" -lt "$END" ]; do
	docker exec digiwal-dev-postgres-1 psql -U custody -d ledger -X -q -At -F'|' -c "$Q" 2>/dev/null
	for c in ledger-service-1 ledger-service-2-1; do
		docker exec digiwal-dev-$c wget -qO- http://127.0.0.1:9100/metrics 2>/dev/null | awk -v t="$(date +%s)" -v c="$c" '
			/^digiwal_db_pool_connections\{/ { match($0, /state="[a-z]+"/); v[substr($0, RSTART+7, RLENGTH-8)] = $NF }
			/^digiwal_db_pool_max_connections/ { m = $NF }
			END { print "POOL|" t "|" c "|" v["acquired"] "|" v["idle"] "|" m }'
	done
	sleep "$INTERVAL"
done
