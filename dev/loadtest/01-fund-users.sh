#!/usr/bin/env bash
# Gives every simulated user ETH on the mock chain (from the well-known Anvil account #0), so withdrawals have funds to draw on.
# The chain-watcher sees the transfers, the ledger credits them, the sweeper moves them to the hot wallet.
#   ETH_PER_USER=5 ./01-fund-users.sh        each accepted withdrawal costs 0.0006 ETH; 5 ETH covers ~8,000 per user
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
ETH_PER_USER=${ETH_PER_USER:-5}
addrs=$(ssh vm3 "cat /var/lib/docker/volumes/digiwal-dev_sim_data/_data/state.json" | python3 -c "import json,sys; print(json.dumps([u['deposit_address'] for u in json.load(sys.stdin)['users'] if u.get('deposit_address')]))")
n=$(echo "$addrs" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
log "funding $n deposit addresses with $ETH_PER_USER ETH each"
cat > /tmp/fund.mjs <<'J'
import { Wallet, JsonRpcProvider, parseEther } from 'ethers';
const p = new JsonRpcProvider(process.env.RPC, undefined, { staticNetwork: true });
const w = new Wallet('0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80', p); // Anvil account #0, a public test key
const addrs = JSON.parse(process.env.ADDRS), value = parseEther(process.env.ETH);
let nonce = await p.getTransactionCount(w.address, 'pending'), last;
for (const a of addrs) last = await p.send('eth_sendRawTransaction', [await w.signTransaction({ type: 2, chainId: 31337, nonce: nonce++, to: a, value, gasLimit: 21000, maxFeePerGas: 10_000_000_000n, maxPriorityFeePerGas: 1_000_000_000n })]);
for (let i = 0; i < 60; i++) { if (await p.getTransactionReceipt(last)) break; await new Promise((r) => setTimeout(r, 2000)); }
console.log('sent', addrs.length);
J
scp -q /tmp/fund.mjs vm3:/tmp/fund.mjs
ssh vm3 "docker run --rm --network host -e RPC=http://10.10.0.2:8545 -e ETH=$ETH_PER_USER -e ADDRS='$addrs' -v /tmp/fund.mjs:/app/fund.mjs digiwal-mockchain:dev node /app/fund.mjs"
log "now wait for the ledger to credit them: ./00-prepare.sh shows the total available ETH"
