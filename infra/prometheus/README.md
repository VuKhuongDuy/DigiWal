# Prometheus

`prometheus.yml` scrapes every service's `:9100/metrics` every 15s and loads `alerts.yml`. There is no Alertmanager and no email for now:
a firing alert shows on http://localhost:9090/alerts and in the Engineering dashboard. History is kept on a volume for two years.

`alerts.yml` has 20 rules (auditor, money, withdrawals, deposits, platform). Thresholds come from the design's Objectives or are marked
"initial" for the Tech Lead to tune. `alerts_test.yml` unit-tests every one with `promtool test rules` (fires, stays quiet, renders its
text); after any change run

    infra/prometheus/check.sh

(it uses the same Prometheus image the stack runs, through Docker).

Not here yet: the gas alerts (`HotGasLow`, `HotGasRunwayLow`, `FeeAtCeiling`, `FeeBumpStorm`) wait for their metrics (A12).
Kafka consumer lag has no alert because no service exports it (design Open Question 8).
