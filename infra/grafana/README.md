# Grafana

Provisioned from files; nothing is configured in the UI (`allowUiUpdates: false`). One datasource (Prometheus, uid `prometheus`),
dashboards under `dashboards/<Folder>/`: a subfolder becomes a Grafana folder.

| Folder | Dashboard | For |
|---|---|---|
| `Business` | `digiwal-business.json` (home page) | the manager: what users do, how it ended, where the money is. Plain language, no service names. |
| `Engineering` | _(A11)_ | the Tech Lead: flows, Sweeper, Reaper, Hot wallet, performance, the auditor's findings. |

Open http://localhost:3000 (anonymous admin in this dev stack).

**Editing a dashboard:** change the JSON (or build it in the UI, export it, and commit the JSON). The Business dashboard is
all-time: every history chart is `increase(<monotonic counter>[1d])` at a 1-day step, so a bar is one UTC day, and the tiles
use `[24h]`. Counters come from the owning service's database at scrape time (`pkg/metrics`), so history survives restarts;
Prometheus keeps it for two years (`infra/docker-compose.yml`). It starts when Prometheus first scrapes: earlier days are empty.

**Not on the dashboard yet:** "time for a deposit to be credited". It needs both chain-watcher's detection time and the ledger's
credit time, which live in different databases; no service exports it, and the auditor (which can read both) does not yet.

**Checked against a real Prometheus and Grafana** (sample series, then every panel query evaluated, the "Funds verified" tile in its
three states, the folder, the datasource and the home page confirmed through Grafana's API), but not yet against the running stack.
