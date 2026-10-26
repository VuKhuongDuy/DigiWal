// k6 scenario through api-gateway: a share of POST /v1/withdrawals (WRITE_PCT, default 20) and the rest reads.
// Reads: 55% balances, 15% deposit-address, 10% whitelist (renormalised to what is left after the writes).
// STEPS: comma list of arrival rates (req/s); each step lasts STEP_S seconds, steps are GAP_S seconds apart.
//   k6 run -e STEPS=100,200,300 -e STEP_S=60 -e WRITE_PCT=20 mix.js
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

// Where the failures come from: 429 is the gateway's own rate limiter (RATE_LIMIT_*_PER_MIN), not a capacity limit.
const limited = new Counter('rate_limited');
const serverErr = new Counter('server_errors');
const tally = (res) => {
  const t = { step: exec.scenario.name };
  if (res.status === 429) limited.add(1, t);
  else if (res.status === 0 || res.status >= 500) serverErr.add(1, t);
};

const keys = JSON.parse(open('/keys.json'));
const base = __ENV.BASE || 'http://127.0.0.1:8084';
const STEPS = (__ENV.STEPS || '100,200,300,400,500').split(',').map(Number);
const STEP_S = parseInt(__ENV.STEP_S || '60');
const GAP_S = parseInt(__ENV.GAP_S || '15');
const WRITE = parseFloat(__ENV.WRITE_PCT || '20') / 100;
const MIX = __ENV.MIX || 'mixed'; // 'balances' sends only GET /v1/balances (api-gateway -> ledger-service -> Postgres ledger)
const ETH = '11111111-1111-1111-1111-111111111111';
const MIN_ETH = '600000000000000'; // 0.0006 ETH, the asset registry's minimum withdrawal

const scenarios = {};
const thresholds = {};
STEPS.forEach((rate, i) => {
  const step = 'r' + rate;
  scenarios[step] = {
    executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: STEP_S + 's', startTime: i * (STEP_S + GAP_S) + 's',
    // k6 initialises every scenario's preAllocatedVUs when the test starts (not when its step starts) and each VU costs ~0.4 MB: with
    // 100+rate/2 pre-allocated and 2*rate allowed per step, five steps up to 4,000 req/s reached 15,000 VUs and 6.4 GB and k6 was OOM-killed.
    // Pre-allocate what a healthy system needs (rate x 0.25 s, capped), allow one VU per request/s (a 1 s latency allowance); k6 starts more when it
    // needs them, but starting thousands at once at the top of a step is slow and shows up as dropped iterations, so do not pre-allocate too few.
    preAllocatedVUs: Math.min(1500, Math.max(100, Math.ceil(rate / 4))), maxVUs: Math.max(200, rate), tags: { step },
  };
  // always-true thresholds exist only to make k6 export these per-step sub-metrics in the summary
  for (const kind of ['wd', 'read']) thresholds[`http_req_duration{step:${step},kind:${kind}}`] = ['p(95)<1e9'];
  thresholds[`http_reqs{step:${step}}`] = ['count>=0'];
  thresholds[`http_req_failed{step:${step}}`] = ['rate<=1'];
  thresholds[`dropped_iterations{scenario:${step}}`] = ['count>=0'];
  thresholds[`rate_limited{step:${step}}`] = ['count>=0'];
  thresholds[`server_errors{step:${step}}`] = ['count>=0'];
});

export const options = { scenarios, thresholds, discardResponseBodies: true, // only the status is used; the bodies were most of each VU's memory
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'] };

export function setup() {
  const users = keys.map((k) => {
    const r = http.get(base + '/v1/whitelist', { headers: { Authorization: 'Bearer ' + k }, responseType: 'text' }); // the only body we read
    const a = r.status === 200 ? r.json().find((w) => w.status === 'active') : null;
    return { k, addr: a ? a.address : null };
  }).filter((u) => u.addr);
  console.log(`SETUP_DONE ${Date.now()} users=${users.length} of ${keys.length}`); // the analyser aligns its windows on this line
  return users;
}

export default function (users) {
  const u = users[Math.floor(Math.random() * users.length)];
  const h = { Authorization: 'Bearer ' + u.k, 'Content-Type': 'application/json' };
  if (MIX === 'balances') { tally(http.get(base + '/v1/balances', { headers: h, tags: { kind: 'read' } })); return; }
  const x = Math.random();
  if (x < WRITE) {
    tally(http.post(base + '/v1/withdrawals', JSON.stringify({
      idempotency_key: 'k6-' + __VU + '-' + __ITER + '-' + Date.now(), asset_id: ETH, amount: MIN_ETH, to_address: u.addr,
    }), { headers: h, tags: { kind: 'wd' }, responseCallback: http.expectedStatuses(200, 202, 422) }));
    return;
  }
  const r = (x - WRITE) / (1 - WRITE); // 0..1 over the read share
  if (r < 0.55 / 0.80) tally(http.get(base + '/v1/balances', { headers: h, tags: { kind: 'read' } }));
  else if (r < 0.70 / 0.80) tally(http.get(base + '/v1/deposit-address', { headers: h, tags: { kind: 'read' } }));
  else tally(http.get(base + '/v1/whitelist', { headers: h, tags: { kind: 'read' } }));
}
