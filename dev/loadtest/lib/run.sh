#!/usr/bin/env bash
# run_load <outdir> <steps> <step_s> <gap_s> <write_pct> [mix]:   mix = mixed (default) or balances (only GET /v1/balances)
# one k6 run on $K6_HOST with the Postgres samplers running beside it.
# Leaves in <outdir>: k6.log, summary.json, sample.ledger.txt (vm2), sample.gateway.txt (vm4), sample.core.txt (vm3).
run_load() {
	local out=$1 steps=$2 step_s=$3 gap_s=$4 write=$5 mix=${6:-mixed}
	local n total
	n=$(echo "$steps" | tr ',' '\n' | wc -l)
	total=$(( n * (step_s + gap_s) + 90 ))
	mkdir -p "$out"
	on_k6 "mkdir -p $REMOTE_DIR/out && rm -f $REMOTE_DIR/out/summary.json" || die "cannot reach $K6_HOST"
	put_k6 "$HERE/k6/mix.js" "$REMOTE_DIR/mix.js"
	on_k6 "test -s $REMOTE_DIR/keys.json" || die "no $REMOTE_DIR/keys.json on $K6_HOST: run 00-prepare.sh first"

	"$HERE/lib/sampler.sh" "$total" digiwal-dev-postgres-1 > "$out/sample.ledger.txt" 2>/dev/null &
	local p1=$!
	ssh "$GATEWAY_NODE" "bash -s $total digiwal-dev-postgres-gateway-1" < "$HERE/lib/sampler.sh" > "$out/sample.gateway.txt" 2>/dev/null &
	local p2=$!
	ssh vm3 "bash -s $total digiwal-dev-postgres-1 withdrawal" < "$HERE/lib/sampler.sh" > "$out/sample.core.txt" 2>/dev/null &
	local p3=$!
	sleep 2

	log "k6: mix=$mix steps=$steps step=${step_s}s gap=${gap_s}s writes=${write}% -> $out"
	on_k6 "docker run --rm --user root --memory 5g --memory-swap 5g --network host -e BASE=$GATEWAY_URL -e STEPS=$steps -e STEP_S=$step_s -e GAP_S=$gap_s -e WRITE_PCT=$write -e MIX=$mix \
		-v $REMOTE_DIR/keys.json:/keys.json:ro -v $REMOTE_DIR/mix.js:/s.js:ro -v $REMOTE_DIR/out:/out \
		grafana/k6 run --summary-export=/out/summary.json /s.js" > "$out/k6.log" 2>&1 || log "k6 exited non-zero (see $out/k6.log); thresholds are informational"
	get_k6 "$REMOTE_DIR/out/summary.json" "$out/summary.json" || die "k6 produced no summary"
	kill "$p1" "$p2" "$p3" 2>/dev/null || true
	wait "$p1" "$p2" "$p3" 2>/dev/null || true
}

# analyse <outdir> <steps> <step_s> <gap_s>: prints and saves the table for one run.
analyse() {
	python3 "$HERE/lib/analyze.py" "$1" "$2" "$3" "$4" | tee "$1/table.txt"
}

# environment_snapshot: what the numbers were measured on, so the report can be honest about it.
environment_snapshot() {
	echo "date: $(date -u +%FT%TZ)"
	echo "code: $(git -C "$HERE/../.." log -1 --format='%h %s' 2>/dev/null || echo unknown)"
	echo "nodes: 3 VMs (vm2 vm3 vm4), each $(nproc) vCPU / $(free -g | awk '/Mem:/{print $2}') GB; chain: mockchain (no EVM); users are simulated"
	for node in vm2 vm3; do
		c=digiwal-dev-postgres-1
		if [ $node = vm2 ]; then docker inspect -f "$node $c cpus={{.HostConfig.NanoCpus}} (0 = no cap) mem={{.HostConfig.Memory}}" $c
		else ssh $node "docker inspect -f '$node $c cpus={{.HostConfig.NanoCpus}} (0 = no cap) mem={{.HostConfig.Memory}}' $c"; fi
	done
	echo "gateway on $GATEWAY_NODE, k6 on $K6_HOST; gateway POSTGRES_MAX_CONNS=$(ssh $GATEWAY_NODE 'docker exec digiwal-dev-api-gateway-1 printenv POSTGRES_MAX_CONNS' 2>/dev/null) ledger-service=$(ssh vm4 'docker exec digiwal-dev-ledger-service-1 printenv POSTGRES_MAX_CONNS' 2>/dev/null)"
	echo "running monitoring containers: $( (docker ps --format '{{.Names}}'; ssh vm3 "docker ps --format '{{.Names}}'"; ssh vm4 "docker ps --format '{{.Names}}'") | grep -cE 'prometheus|grafana|auditor|exporter|cadvisor' )"
}

# wait_idle <seconds>: how long the withdrawal pipeline needs to work off what the test accepted.
wait_idle() {
	local start n; start=$(date +%s)
	while :; do
		n=$(open_withdrawals); [ "${n:-9999}" -lt 20 ] && break
		[ $(( $(date +%s) - start )) -ge "$1" ] && { echo "pipeline backlog after $1 s: $n withdrawals still reserved/validated"; return; }
		sleep 5
	done
	echo "pipeline drained in $(( $(date +%s) - start )) s after the last request"
}
