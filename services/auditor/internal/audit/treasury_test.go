package audit

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/treasury"
	"digiwal/services/auditor/internal/source"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	tHot  = "0x00000000000000000000000000000000000000a1"
	tWarm = "0x00000000000000000000000000000000000000a2"
	tCold = "0x00000000000000000000000000000000000000a3"
	tExt  = "0x00000000000000000000000000000000000000e1"
	tDep  = "0x00000000000000000000000000000000000000a9"
)

type fakeTreasuryChain struct {
	head    int64
	count   uint64
	moves   map[int64][]source.RawMovement
	sent    map[int64][]source.SentTx
	mined   map[string]uint64 // hot tx hashes that exist on chain -> nonce
	blockTm func(n int64) time.Time
}

func (c *fakeTreasuryChain) Head(context.Context) (int64, error) { return c.head, nil }
func (c *fakeTreasuryChain) BlockTime(_ context.Context, n int64) (time.Time, error) {
	return c.blockTm(n), nil
}
func (c *fakeTreasuryChain) TxCount(context.Context, string) (uint64, error) { return c.count, nil }
func (c *fakeTreasuryChain) ScanTreasury(_ context.Context, from, to int64, _ string, _, _ []string) ([]source.RawMovement, []source.SentTx, error) {
	var m []source.RawMovement
	var s []source.SentTx
	for n := from; n <= to; n++ {
		m = append(m, c.moves[n]...)
		s = append(s, c.sent[n]...)
	}
	return m, s, nil
}
func (c *fakeTreasuryChain) Sent(_ context.Context, hash, _ string) (bool, bool, uint64, error) {
	n, ok := c.mined[hash]
	return ok, ok, n, nil
}

type trEnv struct {
	*env
	chain *fakeTreasuryChain
	job   *TreasuryAudit
	key   string
}

// signedTx makes a real signed transaction from a fresh key, so its hash is what hashing the stored raw tx gives.
func signedTx(t *testing.T, nonce uint64) (raw []byte, hash string) {
	t.Helper()
	k, _ := crypto.GenerateKey()
	tx, err := types.SignNewTx(k, types.LatestSignerForChainID(big.NewInt(31337)),
		&types.DynamicFeeTx{ChainID: big.NewInt(31337), Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2), Gas: 21000,
			To: ptr(common.HexToAddress(tExt)), Value: big.NewInt(5)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = tx.MarshalBinary()
	return raw, strings.ToLower(tx.Hash().Hex())
}

func ptr[T any](v T) *T { return &v }

// newTrEnv seeds wallet-service with: next_nonce 3; a withdrawal signature at nonce 0 (request exists) and a
// gas top-up signature at nonce 1. The chain shows both mined, and a withdrawal of 100 out of Hot.
func newTrEnv(t *testing.T) (*trEnv, string, string) {
	e := newEnv(t)
	tr := &trEnv{env: e, chain: &fakeTreasuryChain{head: 100, count: 2, moves: map[int64][]source.RawMovement{}, sent: map[int64][]source.SentTx{},
		mined: map[string]uint64{}, blockTm: func(n int64) time.Time { return t0.Add(time.Duration(n) * time.Second) }}}

	rawW, hashW := signedTx(t, 0)
	rawG, hashG := signedTx(t, 1)
	e.exec(e.wal, `INSERT INTO hot_wallet_nonces (chain_id, next_nonce) VALUES (31337, 3)`)
	e.exec(e.wal, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, nonce, request_hash, signed_raw_tx, caller_cn)
		VALUES ('withdrawal:`+wid+`', 31337, 0, 'h', $1, 'withdrawal-service'), ('gas-topup:31337:0xaa:0', 31337, 1, 'h', $2, 'sweeper')`, rawW, rawG)
	e.exec(e.wd, `INSERT INTO withdrawal_requests (withdrawal_id, user_id, idempotency_key, asset_id, amount, to_address, status, updated_at)
		VALUES ($1, 'u1', 'k1', $2, 100, $3, 'failed', $4)`, wid, assetID, tExt, t0)
	e.exec(e.led, `INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount) VALUES
		($1, 'erc20', $2, 'TKN', 6, 0, 0, 0), ('eeeeeeee-0000-0000-0000-000000000001', 'native', NULL, 'ETH', 18, 0, 0, 0)`, assetID, token)
	e.exec(e.wal, `INSERT INTO wallet_addresses (user_id, derivation_index, address) VALUES ('u1', 1, $1)`, tDep)

	tr.chain.mined[hashW], tr.chain.mined[hashG] = 0, 1
	tr.chain.sent[10] = []source.SentTx{{Hash: hashW, Nonce: 0, Block: 10}}
	tr.chain.sent[11] = []source.SentTx{{Hash: hashG, Nonce: 1, Block: 11}}
	tr.chain.moves[10] = []source.RawMovement{{TxHash: hashW, LogIndex: -1, From: tHot, To: tExt, Amount: big.NewInt(100), Block: 10, Time: t0}}
	tr.chain.moves[11] = []source.RawMovement{{TxHash: hashG, LogIndex: -1, From: tHot, To: tDep, Amount: big.NewInt(1), Block: 11, Time: t0}}

	tr.job = &TreasuryAudit{
		Src:   &source.Treasury{Wallet: e.ro["wallet"], Ledger: e.ro["ledger"], Withdrawal: e.ro["withdrawal"], Chain: tr.chain, ChainID: 31337},
		Store: e.st, Tiers: treasury.Addresses{Hot: tHot, Warm: tWarm, Cold: tCold}, ChainID: 31337,
		Every: 5 * time.Minute, Grace: 10 * time.Minute, ScanBlocks: 30, Now: func() time.Time { return e.now },
	}
	return tr, hashW, hashG
}

func (tr *trEnv) run() job.Result {
	tr.t.Helper()
	res, err := job.RunOnce(context.Background(), tr.st, tr.job, func() time.Time { return tr.now })
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.now = tr.now.Add(5 * time.Minute)
	return res
}

func (tr *trEnv) fs() map[string]found {
	tr.t.Helper()
	rows, err := tr.st.Findings(context.Background(), finding.FlowTreasury)
	if err != nil {
		tr.t.Fatal(err)
	}
	out := map[string]found{}
	for _, r := range rows {
		if r.Status == "open" {
			out[r.Rule+" "+r.Reference] = found{r.Rule, r.Status, r.Severity}
		}
	}
	return out
}

func (tr *trEnv) has(rule string) bool {
	for k := range tr.fs() {
		if strings.HasPrefix(k, rule+" ") {
			return true
		}
	}
	return false
}

func TestHotWalletActingOnlyThroughWalletServiceIsOk(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	for i := 0; i < 2; i++ {
		if res := tr.run(); res != job.OK {
			t.Fatalf("run %d: %s %v", i+1, res, tr.fs())
		}
	}
	mv, err := tr.st.Movements(context.Background())
	if err != nil || len(mv) != 2 {
		t.Fatalf("movements: %v %v", mv, err)
	}
	if mv[0].Kind != "withdrawal" || mv[1].Kind != "gas_topup" || mv[0].Asset != "eeeeeeee-0000-0000-0000-000000000001" {
		t.Errorf("classification: %+v", mv)
	}
}

func TestT1MoreTransactionsThanNoncesAllocated(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	tr.chain.count = 4 // next_nonce is 3
	if res := tr.run(); res != job.Fail || !tr.has("T1") {
		t.Fatalf("%s %v", res, tr.fs())
	}
	tr.chain.count = 3
	if res := tr.run(); res != job.OK {
		t.Fatalf("after: %s %v", res, tr.fs())
	}
}

func TestT2AnotherTxTookASignedNonce(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	rogue := "0x" + strings.Repeat("ab", 32)
	tr.chain.sent[12] = []source.SentTx{{Hash: rogue, Nonce: 1, Block: 12}}
	tr.chain.mined[rogue] = 1
	if res := tr.run(); res != job.Fail || !tr.has("T2") {
		t.Fatalf("%s %v", res, tr.fs())
	}
	// a reorg removes it: the next run re-checks the open reference and resolves it
	delete(tr.chain.mined, rogue)
	if res := tr.run(); res != job.OK {
		t.Fatalf("after the reorg: %s %v", res, tr.fs())
	}
}

func TestT4ValueLeftHotWithoutASignature(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	rogue := "0x" + strings.Repeat("cd", 32)
	tr.chain.sent[12] = []source.SentTx{{Hash: rogue, Nonce: 9, Block: 12}}
	tr.chain.moves[12] = []source.RawMovement{{TxHash: rogue, LogIndex: -1, From: tHot, To: tExt, Amount: big.NewInt(77), Block: 12, Time: t0}}
	tr.chain.mined[rogue] = 9
	if res := tr.run(); res != job.Fail || !tr.has("T4") {
		t.Fatalf("%s %v", res, tr.fs())
	}
	mv, _ := tr.st.Movements(context.Background())
	var kinds []string
	for _, m := range mv {
		kinds = append(kinds, m.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), "unexplained") {
		t.Errorf("the movement should be classed unexplained: %v", kinds)
	}
}

func TestT3SignaturesWithoutAPurpose(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	tr.exec(tr.wal, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, nonce, request_hash, signed_raw_tx, caller_cn)
		VALUES ('mystery:1', 31337, 2, 'h', '\x00', 'sweeper'), ('gas-topup:31337:0xbb:0', 31337, 3, 'h', '\x00', 'withdrawal-service'),
		       ('withdrawal:99999999-9999-9999-9999-999999999999', 31337, 4, 'h', '\x00', 'withdrawal-service')`)
	if res := tr.run(); res != job.Fail {
		t.Fatalf("%s", res)
	}
	n := 0
	for k := range tr.fs() {
		if strings.HasPrefix(k, "T3 ") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("want 3 T3 findings, got %v", tr.fs())
	}
	tr.exec(tr.wal, `DELETE FROM hot_wallet_signatures WHERE idempotency_key IN ('mystery:1', 'gas-topup:31337:0xbb:0', 'withdrawal:99999999-9999-9999-9999-999999999999')`)
	if res := tr.run(); res != job.OK {
		t.Fatalf("after: %s %v", res, tr.fs())
	}
}

func TestT5WarmAndColdMovements(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	h := func(c string) string { return "0x" + strings.Repeat(c, 32) }
	tr.chain.moves[20] = []source.RawMovement{
		{TxHash: h("01"), LogIndex: -1, From: tWarm, To: tHot, Amount: big.NewInt(10), Block: 20, Time: t0},             // info
		{TxHash: h("02"), LogIndex: 4, Token: token, From: tCold, To: tExt, Amount: big.NewInt(3), Block: 20, Time: t0}, // warn
		{TxHash: h("03"), LogIndex: -1, From: tHot, To: tWarm, Amount: big.NewInt(9), Block: 20, Time: t0},              // not T5
	}
	tr.chain.sent[20] = nil
	// the Hot -> Warm tx is a signed Hot tx in real life; here it is not Hot-sent, so only the movement matters
	res := tr.run()
	if res != job.Warn {
		t.Fatalf("a Cold -> external movement warns: %s %v", res, tr.fs())
	}
	sev := map[string]string{}
	for k, v := range tr.fs() {
		sev[k] = v.severity
	}
	var info, warn int
	for k, s := range sev {
		if strings.HasPrefix(k, "T5 ") {
			if s == "info" {
				info++
			}
			if s == "warn" {
				warn++
			}
		}
	}
	if info != 1 || warn != 1 {
		t.Fatalf("T5: %d info %d warn: %v", info, warn, sev)
	}
	mv, _ := tr.st.Movements(context.Background())
	kinds := map[string]bool{}
	for _, m := range mv {
		kinds[m.Kind] = true
	}
	for _, k := range []string{"warm_to_hot", "hot_to_warm", "unexplained"} {
		if !kinds[k] {
			t.Errorf("movement kind %q not recorded: %v", k, kinds)
		}
	}
}

func TestTreasuryScanWaitsForConfirmationsAndGrace(t *testing.T) {
	tr, _, _ := newTrEnv(t)
	tr.chain.head = 11 // limit = head - 2 = 9: nothing of interest is deep enough yet
	tr.run()
	if cur, _ := tr.st.Cursor(context.Background(), cursorTreasury); cur != "9" {
		t.Fatalf("cursor %q, want 9 (head - confirmations)", cur)
	}
	if mv, _ := tr.st.Movements(context.Background()); len(mv) != 0 {
		t.Fatalf("movements before they are deep enough: %v", mv)
	}
	tr.chain.head = 100
	tr.chain.blockTm = func(n int64) time.Time { return tr.now.Add(time.Duration(n-100) * time.Minute) } // only old blocks pass the Grace Period
	tr.run()
	cur, _ := tr.st.Cursor(context.Background(), cursorTreasury)
	if cur == "98" || cur == "9" {
		t.Fatalf("the scan must stop inside the Grace Period: cursor %q", cur)
	}
}
