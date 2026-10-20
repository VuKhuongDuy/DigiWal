// mockchain: a small Ethereum JSON-RPC node standing in for Anvil in the dev environment.
// It runs no EVM. It understands native transfers and the one contract the environment deploys (MockERC20: balanceOf,
// transfer, Transfer events), mines a block every BLOCK_TIME_MS, and answers the calls the services make
// (docs: e2e/mockchain/README.md). State lives in memory and is rebuilt on start from an append-only journal that is
// fsynced per block; a partial last line is cut off, so a kill at any moment cannot make the chain unreadable
// (Anvil's --state file is rewritten in place and a kill mid-dump corrupts it).
import http from 'node:http';
import fs from 'node:fs';
import {
  Transaction, HDNodeWallet, Mnemonic, getCreateAddress, keccak256, encodeRlp, getAddress, AbiCoder, id as topicId, concat, zeroPadValue, toBeHex,
} from 'ethers';

const PORT = +(process.env.PORT || 8545);
const DATA_DIR = process.env.DATA_DIR || './data';
const BLOCK_TIME_MS = +(process.env.BLOCK_TIME_MS || 2000);
const CHAIN_ID = BigInt(process.env.CHAIN_ID || 31337);
const BIN = fs.readFileSync(process.env.MOCKERC20_BIN || '../contracts/MockERC20.bin', 'utf8').trim().replace(/^0x/, '').toLowerCase();
const GAS_LIMIT = 30_000_000n;
const BASE_FEE_DEFAULT = 1_000_000_000n;
const TIP_DEFAULT = 1_000_000_000n;
const COINBASE = '0x0000000000000000000000000000000000000000';
const EMPTY_ROOT = '0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421';
const EMPTY_UNCLES = '0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347';
const ZERO32 = '0x' + '00'.repeat(32);
const BLOOM0 = '0x' + '00'.repeat(256);
const TRANSFER_TOPIC = topicId('Transfer(address,address,uint256)');
const SEL = { transfer: '0xa9059cbb', balanceOf: '0x70a08231', decimals: '0x313ce567', symbol: '0x95d89b41', name: '0x06fdde03', totalSupply: '0x18160ddd' };
const abi = AbiCoder.defaultAbiCoder();

const q = (n) => '0x' + BigInt(n).toString(16);                 // JSON-RPC quantity
const lc = (a) => a.toLowerCase();
const rlpInt = (n) => { n = BigInt(n); if (n === 0n) return '0x'; let h = n.toString(16); if (h.length % 2) h = '0' + h; return '0x' + h; };

// ---------------------------------------------------------------- state
let accounts, tokens, blocks, byHash, txIndex, mempool, nextBaseFee, journalFd;
function resetState() {
  accounts = new Map();      // addr -> {balance, nonce}
  tokens = new Map();        // contract addr -> {supply, balances: Map}
  blocks = [];               // block objects (index = number)
  byHash = new Map();
  txIndex = new Map();       // tx hash -> {block, i}
  mempool = new Map();       // tx hash -> {tx, raw}
  nextBaseFee = null;
}
const acct = (a) => { a = lc(a); let x = accounts.get(a); if (!x) { x = { balance: 0n, nonce: 0 }; accounts.set(a, x); } return x; };
const tokenBal = (t, a) => t.balances.get(lc(a)) ?? 0n;

function headerHash(h) {
  return keccak256(encodeRlp([
    h.parentHash, EMPTY_UNCLES, h.miner, h.stateRoot, h.transactionsRoot, h.receiptsRoot, BLOOM0, rlpInt(0), rlpInt(h.number),
    rlpInt(GAS_LIMIT), rlpInt(h.gasUsed), rlpInt(h.timestamp), '0x', ZERO32, '0x0000000000000000', rlpInt(h.baseFee),
  ]));
}
// the ten well-known Anvil accounts (mnemonic "test ... junk", m/44'/60'/0'/0/i) hold 10000 ETH each
function fundDefaults() {
  const m = Mnemonic.fromPhrase('test test test test test test test test test test test junk');
  for (let i = 0; i < 10; i++) acct(HDNodeWallet.fromMnemonic(m, `m/44'/60'/0'/0/${i}`).address).balance = 10_000n * 10n ** 18n;
}

// ---------------------------------------------------------------- execution
const effPrice = (tx, base) => tx.maxFeePerGas != null ? (tx.maxFeePerGas < base + tx.maxPriorityFeePerGas ? tx.maxFeePerGas : base + tx.maxPriorityFeePerGas) : tx.gasPrice;
const tipOf = (tx, base) => effPrice(tx, base) - base;
const logOf = (addr, from, to, v) => ({ address: addr, topics: [TRANSFER_TOPIC, zeroPadValue(from, 32), zeroPadValue(to, 32)], data: toBeHex(v, 32) });
const revertData = (msg) => '0x08c379a0' + abi.encode(['string'], [msg]).slice(2);

// run the call part of a tx against state (mutating); returns {ok, gas, logs, contract, revert}
function runCall(from, tx) {
  const to = tx.to ? lc(tx.to) : null, data = tx.data || '0x', value = tx.value;
  if (to === null) {
    const d = lc(data).replace(/^0x/, '');
    if (!d.startsWith(BIN) || d.length !== BIN.length + 64) return { ok: false, gas: 100_000n, logs: [], revert: 'unsupported contract' };
    const supply = BigInt('0x' + d.slice(BIN.length));
    const addr = lc(getCreateAddress({ from, nonce: acct(from).nonce }));
    tokens.set(addr, { supply, balances: new Map([[lc(from), supply]]) });
    return { ok: true, gas: 700_000n, logs: [logOf(addr, '0x' + '00'.repeat(20), from, supply)], contract: getAddress(addr) };
  }
  const tok = tokens.get(to);
  if (!tok) { acct(to).balance += value; acct(from).balance -= value; return { ok: true, gas: 21_000n, logs: [] }; }
  if (value > 0n) return { ok: false, gas: 30_000n, logs: [], revert: 'not payable' };
  if (lc(data).startsWith(SEL.transfer) && data.length >= 10 + 128) {
    const toA = '0x' + data.slice(34, 74), amt = BigInt('0x' + data.slice(74, 138));
    if (tokenBal(tok, from) < amt) return { ok: false, gas: 30_000n, logs: [], revert: 'insufficient balance' };
    tok.balances.set(lc(from), tokenBal(tok, from) - amt);
    tok.balances.set(lc(toA), tokenBal(tok, toA) + amt);
    return { ok: true, gas: 51_000n, logs: [logOf(to, from, toA, amt)] };
  }
  return { ok: true, gas: 25_000n, logs: [] };
}

// ---------------------------------------------------------------- mining
function txView(tx, raw) {
  return {
    type: tx.type ?? 0, hash: tx.hash, from: lc(tx.from), to: tx.to ? lc(tx.to) : null, nonce: tx.nonce, gas: tx.gasLimit, value: tx.value, data: tx.data,
    maxFeePerGas: tx.maxFeePerGas, maxPriorityFeePerGas: tx.maxPriorityFeePerGas, gasPrice: tx.gasPrice, chainId: tx.chainId,
    accessList: tx.accessList ?? [], sig: tx.signature, raw,
  };
}
function mineBlock(rawList = null, ts = null) {
  const number = blocks.length, parent = blocks[number - 1];
  const base = nextBaseFee ?? BASE_FEE_DEFAULT; nextBaseFee = null;
  const timestamp = ts ?? Math.max(Math.floor(Date.now() / 1000), parent ? parent.header.timestamp + 1 : 0);
  const included = [];
  const pick = () => {  // next executable tx: the lowest-nonce tx of each sender, highest tip first
    let best = null;
    const bySender = new Map();
    for (const e of mempool.values()) { const f = lc(e.tx.from); const cur = bySender.get(f); if (e.tx.nonce === acct(f).nonce && (!cur || tipOf(e.tx, base) > tipOf(cur.tx, base))) bySender.set(f, e); }
    for (const e of bySender.values()) if (!best || tipOf(e.tx, base) > tipOf(best.tx, base)) best = e;
    return best;
  };
  let gasUsed = 0n;
  const execOne = (e) => {
    const from = lc(e.tx.from), a = acct(from), price = effPrice(e.tx, base);
    if (e.tx.nonce !== a.nonce) return false;
    if (price < base || a.balance < e.tx.gasLimit * price + e.tx.value) return false;
    let r; try { r = runCall(from, e.tx); } catch (err) { console.error('call failed:', err.message); r = { ok: false, gas: 21_000n, logs: [] }; }
    const used = r.gas > e.tx.gasLimit ? e.tx.gasLimit : r.gas;
    if (r.ok === false) { /* state untouched by a revert */ }
    a.nonce += 1; a.balance -= used * price; acct(COINBASE).balance += used * (price - base);
    gasUsed += used;
    included.push({ e, r, used, price });
    return true;
  };
  if (rawList) {
    for (const raw of rawList) { const tx = Transaction.from(raw); const e = { tx, raw }; mempool.delete(tx.hash); execOne(e); }
  } else {
    for (let e = pick(); e && gasUsed < GAS_LIMIT - 1_000_000n; e = pick()) { mempool.delete(e.tx.hash); if (!execOne(e)) { /* became unexecutable */ } }
    // drop txs that can never run (nonce already used)
    for (const [h, e] of mempool) if (e.tx.nonce < acct(e.tx.from).nonce) mempool.delete(h);
  }
  const txs = included.map((x) => x.e.tx.hash);
  const header = {
    number, parentHash: parent ? parent.hash : ZERO32, miner: COINBASE, timestamp, gasUsed, baseFee: base,
    stateRoot: keccak256(concat([ZERO32, toBeHex(number, 32)])),
    transactionsRoot: txs.length ? keccak256(concat(txs)) : EMPTY_ROOT,
    receiptsRoot: txs.length ? keccak256(concat(txs.map((h) => keccak256(h)))) : EMPTY_ROOT,
  };
  const hash = headerHash(header);
  let cum = 0n, logIndex = 0;
  const blk = { number, hash, header, txs: [], raws: included.map((x) => x.e.raw) };
  included.forEach((x, i) => {
    cum += x.used;
    const logs = x.r.logs.map((l) => ({ ...l, blockNumber: q(number), blockHash: hash, transactionHash: x.e.tx.hash, transactionIndex: q(i), logIndex: q(logIndex++), removed: false }));
    const view = txView(x.e.tx, x.e.raw);
    const receipt = {
      transactionHash: x.e.tx.hash, transactionIndex: q(i), blockHash: hash, blockNumber: q(number), from: view.from, to: view.to,
      cumulativeGasUsed: q(cum), gasUsed: q(x.used), contractAddress: x.r.contract ?? null, logs, logsBloom: BLOOM0, status: x.r.ok ? '0x1' : '0x0',
      effectiveGasPrice: q(x.price), type: q(view.type),
    };
    blk.txs.push({ view, receipt, price: x.price });
    txIndex.set(x.e.tx.hash, { block: number, i });
  });
  blocks.push(blk); byHash.set(hash, number);
  return blk;
}

// ---------------------------------------------------------------- journal
function journalOpen() {
  fs.mkdirSync(DATA_DIR, { recursive: true });
  const p = `${DATA_DIR}/chain.jsonl`;
  resetState(); fundDefaults();
  const text = fs.existsSync(p) ? fs.readFileSync(p, 'utf8') : '';
  const lines = text.split('\n'); lines.pop();           // '' after a well-formed file, a partial last line otherwise
  let good = 0, genesisTs = null;
  for (const line of lines) {
    let j; try { j = JSON.parse(line); } catch { break; }
    if (j.op === 'genesis') { genesisTs = j.ts; mineBlock([], j.ts); }
    else if (j.op === 'balance') acct(j.addr).balance = BigInt(j.value);
    else if (j.op === 'basefee') nextBaseFee = BigInt(j.value);
    else if (j.op === 'block') mineBlock(j.raws, j.ts);
    good += Buffer.byteLength(line) + 1;
  }
  const size = Buffer.byteLength(text);
  if (good < size) { fs.truncateSync(p, good); console.log(`journal: cut a partial tail (${size - good} bytes)`); }
  journalFd = fs.openSync(p, 'a');
  if (genesisTs === null) { genesisTs = Math.floor(Date.now() / 1000); mineBlock([], genesisTs); journal({ op: 'genesis', ts: genesisTs }); }
  console.log(`chain restored at block ${blocks.length - 1}`);
}
const journal = (obj) => { fs.writeSync(journalFd, JSON.stringify(obj) + '\n'); fs.fsyncSync(journalFd); };
function mineAndJournal() {
  const blk = mineBlock();
  journal({ op: 'block', ts: blk.header.timestamp, raws: blk.raws });
  return blk;
}

// ---------------------------------------------------------------- JSON views
const blockNum = (tag) => {
  if (tag == null || tag === 'latest' || tag === 'pending' || tag === 'safe' || tag === 'finalized') return blocks.length - 1;
  if (tag === 'earliest') return 0;
  return Number(BigInt(tag));
};
function txJson(t, blk, i) {
  const v = t.view, s = v.sig;
  const o = {
    hash: v.hash, nonce: q(v.nonce), blockHash: blk ? blk.hash : null, blockNumber: blk ? q(blk.number) : null, transactionIndex: blk ? q(i) : null,
    from: v.from, to: v.to, value: q(v.value), gas: q(v.gas), gasPrice: q(blk ? t.price : (v.maxFeePerGas ?? v.gasPrice)), input: v.data,
    v: q(v.type === 0 ? (v.chainId ? v.chainId * 2n + 35n + BigInt(s.yParity) : BigInt(s.v)) : s.yParity), r: s.r, s: s.s, type: q(v.type), chainId: q(v.chainId),
  };
  if (v.type === 2) { o.maxFeePerGas = q(v.maxFeePerGas); o.maxPriorityFeePerGas = q(v.maxPriorityFeePerGas); o.accessList = v.accessList; o.yParity = q(s.yParity); }
  if (v.type === 1) { o.accessList = v.accessList; o.yParity = q(s.yParity); }
  return o;
}
function blockJson(blk, full) {
  const h = blk.header;
  return {
    number: q(blk.number), hash: blk.hash, parentHash: h.parentHash, nonce: '0x0000000000000000', sha3Uncles: EMPTY_UNCLES, logsBloom: BLOOM0,
    transactionsRoot: h.transactionsRoot, stateRoot: h.stateRoot, receiptsRoot: h.receiptsRoot, miner: h.miner, difficulty: '0x0', totalDifficulty: '0x0',
    extraData: '0x', size: q(500 + 200 * blk.txs.length), gasLimit: q(GAS_LIMIT), gasUsed: q(h.gasUsed), timestamp: q(h.timestamp), mixHash: ZERO32,
    baseFeePerGas: q(h.baseFee), uncles: [],
    transactions: blk.txs.map((t, i) => (full ? txJson(t, blk, i) : t.view.hash)),
  };
}
const matchTopic = (want, got) => want == null || (Array.isArray(want) ? want.some((w) => w == null || lc(w) === lc(got ?? '')) : lc(want) === lc(got ?? ''));

// ---------------------------------------------------------------- RPC
const rpcErr = (code, message, data) => Object.assign(new Error(message), { rpcCode: code, rpcData: data });
function callResult(call) {   // eth_call / estimateGas dry-run on a copy of the state
  const to = call.to ? lc(call.to) : null, data = lc(call.data ?? call.input ?? '0x');
  const tok = to && tokens.get(to);
  if (tok) {
    if (data.startsWith(SEL.balanceOf)) return abi.encode(['uint256'], [tokenBal(tok, '0x' + data.slice(34, 74))]);
    if (data.startsWith(SEL.decimals)) return abi.encode(['uint8'], [18]);
    if (data.startsWith(SEL.totalSupply)) return abi.encode(['uint256'], [tok.supply]);
    if (data.startsWith(SEL.symbol)) return abi.encode(['string'], ['TT']);
    if (data.startsWith(SEL.name)) return abi.encode(['string'], ['Test Token']);
    if (data.startsWith(SEL.transfer)) {
      const from = call.from ?? '0x' + '00'.repeat(20);
      const amt = BigInt('0x' + data.slice(74, 138));
      if (tokenBal(tok, from) < amt) throw rpcErr(3, 'execution reverted: insufficient balance', revertData('insufficient balance'));
      return abi.encode(['bool'], [true]);
    }
  }
  return '0x';
}
const methods = {
  web3_clientVersion: () => 'mockchain/1.0',
  net_version: () => String(CHAIN_ID),
  eth_chainId: () => q(CHAIN_ID),
  eth_syncing: () => false,
  eth_accounts: () => [],
  eth_blockNumber: () => q(blocks.length - 1),
  eth_gasPrice: () => q((nextBaseFee ?? BASE_FEE_DEFAULT) + TIP_DEFAULT),
  eth_maxPriorityFeePerGas: () => q(TIP_DEFAULT),
  eth_feeHistory: ([n]) => { const c = Number(BigInt(n)); const last = blocks.length - 1; return { oldestBlock: q(Math.max(0, last - c + 1)), baseFeePerGas: Array(c + 1).fill(q(BASE_FEE_DEFAULT)), gasUsedRatio: Array(c).fill(0), reward: Array(c).fill([q(TIP_DEFAULT)]) }; },
  eth_getBalance: ([a]) => q(acct(a).balance),
  eth_getTransactionCount: ([a, tag]) => {
    let n = acct(a).nonce;
    if (tag === 'pending') { const mine = [...mempool.values()].filter((e) => lc(e.tx.from) === lc(a)).map((e) => e.tx.nonce); while (mine.includes(n)) n++; }
    return q(n);
  },
  eth_getCode: ([a]) => (tokens.has(lc(a)) ? '0x' + BIN.slice(0, 64) : '0x'),
  eth_getBlockByNumber: ([tag, full]) => { const b = blocks[blockNum(tag)]; return b ? blockJson(b, full) : null; },
  eth_getBlockByHash: ([h, full]) => { const n = byHash.get(h); return n == null ? null : blockJson(blocks[n], full); },
  eth_getTransactionByHash: ([h]) => {
    const ix = txIndex.get(h); if (ix) { const b = blocks[ix.block]; return txJson(b.txs[ix.i], b, ix.i); }
    const p = mempool.get(h); return p ? txJson({ view: txView(p.tx, p.raw) }, null, 0) : null;
  },
  eth_getTransactionReceipt: ([h]) => { const ix = txIndex.get(h); return ix ? blocks[ix.block].txs[ix.i].receipt : null; },
  eth_getLogs: ([f]) => {
    const from = blockNum(f.fromBlock ?? 'latest'), to = blockNum(f.toBlock ?? 'latest');
    const addrs = f.address == null ? null : (Array.isArray(f.address) ? f.address : [f.address]).map(lc);
    const out = [];
    for (let n = Math.max(0, from); n <= Math.min(to, blocks.length - 1); n++) for (const t of blocks[n].txs) for (const l of t.receipt.logs) {
      if (addrs && !addrs.includes(lc(l.address))) continue;
      if (f.topics && !f.topics.every((w, k) => matchTopic(w, l.topics[k]))) continue;
      out.push(l);
    }
    return out;
  },
  eth_call: ([call]) => callResult(call),
  eth_estimateGas: ([call]) => {
    const data = lc(call.data ?? call.input ?? '0x');
    if (!call.to) return q(800_000);
    if (tokens.has(lc(call.to)) && data.startsWith(SEL.transfer)) { callResult({ ...call, data }); return q(51_000); }
    return q(data !== '0x' && data !== '' ? 60_000 : 21_000);
  },
  eth_sendRawTransaction: ([raw]) => {
    let tx; try { tx = Transaction.from(raw); } catch (e) { throw rpcErr(-32000, 'invalid transaction: ' + e.message); }
    const from = lc(tx.from), a = acct(from), base = nextBaseFee ?? BASE_FEE_DEFAULT;
    if (tx.chainId !== CHAIN_ID) throw rpcErr(-32000, 'invalid chain id for signer');
    if (mempool.has(tx.hash) || txIndex.has(tx.hash)) throw rpcErr(-32000, 'already known');
    if (tx.nonce < a.nonce) throw rpcErr(-32000, `nonce too low: next nonce ${a.nonce}, tx nonce ${tx.nonce}`);
    const price = tx.maxFeePerGas ?? tx.gasPrice;
    if (price < base) throw rpcErr(-32000, 'max fee per gas less than block base fee');
    if (a.balance < tx.gasLimit * price + tx.value) throw rpcErr(-32000, 'insufficient funds for gas * price + value');
    for (const [h, e] of mempool) if (lc(e.tx.from) === from && e.tx.nonce === tx.nonce) {
      const oldMax = e.tx.maxFeePerGas ?? e.tx.gasPrice, oldTip = e.tx.maxPriorityFeePerGas ?? e.tx.gasPrice;
      const newTip = tx.maxPriorityFeePerGas ?? tx.gasPrice;
      if (price * 100n < oldMax * 110n || newTip * 100n < oldTip * 110n) throw rpcErr(-32000, 'replacement transaction underpriced');
      mempool.delete(h);
    }
    mempool.set(tx.hash, { tx, raw });
    return tx.hash;
  },
  anvil_mine: ([n]) => { for (let i = 0; i < Number(BigInt(n ?? 1)); i++) mineAndJournal(); return null; },
  evm_mine: () => { mineAndJournal(); return '0x0'; },
  anvil_setBalance: ([a, v]) => { acct(a).balance = BigInt(v); journal({ op: 'balance', addr: lc(a), value: BigInt(v).toString() }); return null; },
  anvil_setNextBlockBaseFeePerGas: ([v]) => { nextBaseFee = BigInt(v); journal({ op: 'basefee', value: BigInt(v).toString() }); return null; },
  evm_setAutomine: () => null,
  evm_setIntervalMining: () => null,
};

function handle(req) {
  const fn = methods[req.method];
  if (!fn) return { jsonrpc: '2.0', id: req.id, error: { code: -32601, message: `the method ${req.method} does not exist/is not available` } };
  try { return { jsonrpc: '2.0', id: req.id, result: fn(req.params ?? []) }; }
  catch (e) { return { jsonrpc: '2.0', id: req.id, error: { code: e.rpcCode ?? -32603, message: e.message, ...(e.rpcData ? { data: e.rpcData } : {}) } }; }
}

journalOpen();
http.createServer((rq, rs) => {
  if (rq.method === 'GET') { rs.writeHead(200); rs.end(`ok block ${blocks.length - 1}\n`); return; }
  let body = '';
  rq.on('data', (c) => { body += c; if (body.length > 4_000_000) rq.destroy(); });
  rq.on('end', () => {
    let msg; try { msg = JSON.parse(body); } catch { rs.writeHead(400); rs.end(); return; }
    const out = Array.isArray(msg) ? msg.map(handle) : handle(msg);
    rs.writeHead(200, { 'content-type': 'application/json' }); rs.end(JSON.stringify(out));
  });
}).listen(PORT, '0.0.0.0', () => console.log(`mockchain listening on :${PORT}, chain id ${CHAIN_ID}, block time ${BLOCK_TIME_MS} ms`));
setInterval(() => { try { mineAndJournal(); } catch (e) { console.error('mine failed:', e); } }, BLOCK_TIME_MS);
process.on('SIGTERM', () => process.exit(0));
