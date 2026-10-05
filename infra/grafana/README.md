# Grafana

Provisioned from files; nothing is configured in the UI (`allowUiUpdates: false`). One datasource (Prometheus, uid `prometheus`),
dashboards under `dashboards/<Folder>/`: a subfolder becomes a Grafana folder.

| Folder | Dashboard | For |
|---|---|---|
| `Business` | `digiwal-business.json` (home page) | the manager: what users do, how it ended, where the money is. Plain language, no service names. |
| `Engineering` | `digiwal-engineering.json` | the Tech Lead: firing alerts, the auditor's findings, deposit and withdraw flow, Sweeper, Reaper, wallets, performance. |

Open http://localhost:3000 (anonymous admin in this dev stack).

**Editing a dashboard:** change the JSON (or build it in the UI, export it, and commit the JSON). The Business dashboard is
all-time: every history chart is `increase(<monotonic counter>[1d])` at a 1-day step, so a bar is one UTC day, and the tiles
use `[24h]`. Counters come from the owning service's database at scrape time (`pkg/metrics`), so history survives restarts;
Prometheus keeps it for two years (`infra/docker-compose.yml`). It starts when Prometheus first scrapes: earlier days are empty.

**Not on the dashboard yet:** "time for a deposit to be credited". It needs both chain-watcher's detection time and the ledger's
credit time, which live in different databases; no service exports it, and the auditor (which can read both) does not yet.

**Checked against a real Prometheus and Grafana** (sample series, then every panel query evaluated, the "Funds verified" tile in its
three states, the folder, the datasource and the home page confirmed through Grafana's API), but not yet against the running stack.

**The Engineering dashboard** has one extra datasource, `Auditor (read-only)`, a Postgres login (`grafana_ro`) on the auditor's own database. It is
the single exception to "Grafana reads only Prometheus" (reconciliation-audit.md, Amendments 2): it can `SELECT` four tables (suspicious
transactions, treasury movements, run history, reconciliation figures), nothing else and no writes (`services/auditor/roles/grafana.sql`, tested), and
only this dashboard uses it. Create the login with `make -C services/auditor grafana-role` once the auditor has started. Without it only those four
tables' panels show an error; the rest work.

Numbers marked *(process)* restart with their service (sweeper keeps no database; the stage histogram, Reaper and ALERT counters are in-process).

**Not on the Engineering dashboard yet:** gas and Fee Bump panels (A12), request-to-credit latency, RPC latency, Kafka consumer lag and Hot coverage
(no service exports these; coverage is the auditor's C3 finding instead).

**Checked** against a real Prometheus (the real `alerts.yml`, sample series for every metric the dashboard uses), a real Postgres (the auditor's real
migrations and `grafana.sql`) and a real Grafana: both dashboards load into their folders, the datasource reports "Database Connection OK", and all 61
panel queries (57 PromQL, 4 SQL, the SQL through Grafana) run and return rows. Not yet against the running dev stack.
