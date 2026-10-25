#!/usr/bin/env bash
# Lint and unit-test the Prometheus alert rules with promtool (from the same image the stack runs).
#   infra/prometheus/check.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
img=prom/prometheus:v2.55.1
run() { docker run --rm --entrypoint promtool -v "$here:/p:ro" "$img" "$@"; }
run check rules /p/alerts.yml
run test rules /p/alerts_test.yml
run check rules /p/alerts-node.yml
run test rules /p/alerts-node_test.yml
# the multi-node config names its rule files by their path inside the container
docker run --rm --entrypoint promtool -v "$here/prometheus.multi-node.yml:/etc/prometheus/prometheus.yml:ro" -v "$here/alerts.yml:/etc/prometheus/alerts.yml:ro" -v "$here/alerts-node.yml:/etc/prometheus/alerts-node.yml:ro" "$img" check config /etc/prometheus/prometheus.yml
