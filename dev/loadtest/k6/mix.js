// k6 scenario through api-gateway: a share of POST /v1/withdrawals (WRITE_PCT, default 20) and the rest reads.
// Reads: 55% balances, 15% deposit-address, 10% whitelist (renormalised to what is left after the writes).
// STEPS: comma list of arrival rates (req/s); each step lasts STEP_S seconds, steps are GAP_S seconds apart.
//   k6 run -e STEPS=100,200,300 -e STEP_S=60 -e WRITE_PCT=20 mix.js
import http from 'k6/http';

const keys = JSON.parse(open('/keys.json'));
const base = __ENV.BASE || 'http://127.0.0.1:8084';
const STEPS = (__ENV.STEPS || '100,200,300,400,500').split(',').map(Number);
const STEP_S = parseInt(__ENV.STEP_S || '60');
const GAP_S = parseInt(__ENV.GAP_S || '15');
const WRITE = parseFloat(__ENV.WRITE_PCT || '20') / 100;
const ETH = '11111111-1111-1111-1111-111111111111';
const MIN_ETH = '600000000000000'; // 0.0006 ETH, the asset registry's minimum withdrawal

const scenarios = {};
const thresholds = {};
STEPS.forEach((rate, i) => {
  const step = 'r' + rate;
  scenarios[step] = {
    executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: STEP_S + 's', startTime: i * (STEP_S + GAP_S) + 's',
    preAllocatedVUs: Math.max(100, Math.ceil(rate / 2)), maxVUs: Math.max(600, rate * 2), tags: { step },
  };
  // always-true thresholds exist only to make k6 export these per-step sub-metrics in the summary
  for (const kind of ['wd', 'read']) thresholds[`http_req_duration{step:${step},kind:${kind}}`] = ['p(95)<1e9'];
  thresholds[`http_reqs{step:${step}}`] = ['count>=0'];
  thresholds[`http_req_failed{step:${step}}`] = ['rate<=1'];
  thresholds[`dropped_iterations{scenario:${step}}`] = ['count>=0'];
});

export const options = { scenarios, thresholds, summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'] };

export function setup() {
  const users = keys.map((k) => {
    const r = http.get(base + '/v1/whitelist', { headers: { Authorization: 'Bearer ' + k } });
    const a = r.status === 200 ? r.json().find((w) => w.status === 'active') : null;
    return { k, addr: a ? a.address : null };
  }).filter((u) => u.addr);
  console.log(`SETUP_DONE ${Date.now()} users=${users.length} of ${keys.length}`); // the analyser aligns its windows on this line
  return users;
}

export default function (users) {
  const u = users[Math.floor(Math.random() * users.length)];
  const h = { Authorization: 'Bearer ' + u.k, 'Content-Type': 'application/json' };
  const x = Math.random();
  if (x < WRITE) {
    http.post(base + '/v1/withdrawals', JSON.stringify({
      idempotency_key: 'k6-' + __VU + '-' + __ITER + '-' + Date.now(), asset_id: ETH, amount: MIN_ETH, to_address: u.addr,
    }), { headers: h, tags: { kind: 'wd' }, responseCallback: http.expectedStatuses(200, 202, 422) });
    return;
  }
  const r = (x - WRITE) / (1 - WRITE); // 0..1 over the read share
  if (r < 0.55 / 0.80) http.get(base + '/v1/balances', { headers: h, tags: { kind: 'read' } });
  else if (r < 0.70 / 0.80) http.get(base + '/v1/deposit-address', { headers: h, tags: { kind: 'read' } });
  else http.get(base + '/v1/whitelist', { headers: h, tags: { kind: 'read' } });
}
