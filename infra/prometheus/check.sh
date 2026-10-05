#!/usr/bin/env bash
# Lint and unit-test the Prometheus alert rules with promtool (from the same image the stack runs).
#   infra/prometheus/check.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
img=prom/prometheus:v2.55.1
run() { docker run --rm --entrypoint promtool -v "$here:/p:ro" "$img" "$@"; }
run check rules /p/alerts.yml
run test rules /p/alerts_test.yml
