package audit

import (
	"context"
	"math/big"
	"testing"
	"time"

	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/deposit"
	"digiwal/services/auditor/internal/source"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	dChain = 31337
	dTx    = "0xddd0000000000000000000000000000000000000000000000000000000000001"
	dLog   = 3
	dBlock = 10
	dHash  = "0xb10c000000000000000000000000000000000000000000000000000000000010"
	depAdr = "0x00000000000000000000000000000000000000a1"
)

var dRef = source.DepositKey(dChain, dTx, dLog)

type fakeDepChain struct {
	transfers map[string]deposit.OnChain
	cands     map[int64][]source.Candidate // block -> transfers in it
	blockTime func(n int64) time.Time
	scans     [][2]int64
}

func (c *fakeDepChain) Transfer(_ context.Context, tx string, log int64) (deposit.OnChain, error) {
	return c.transfers[source.DepositKey(dChain, tx, log)], nil
}
func (c *fakeDepChain) BlockTime(_ context.Context, n int64) (time.Time, error) {
	return c.blockTime(n), nil
}
func (c *fakeDepChain) Scan(_ context.Context, from, to int64, _ []string, _ func(string) bool) ([]source.Candidate, error) {
	c.scans = append(c.scans, [2]int64{from, to})
	var out []source.Candidate
	for n := from; n <= to; n++ {
		out = append(out, c.cands[n]...)
	}
	return out, nil
}

type depEnv struct {
	*env
	chain *fakeDepChain
	job   *DepositAudit
}

// newDepEnv seeds one consistent confirmed, credited ERC-20 deposit of 500 into every store.
func newDepEnv(t *testing.T) *depEnv {
	e := newEnv(t)
	d := &depEnv{env: e, chain: &fakeDepChain{
		transfers: map[string]deposit.OnChain{},
		cands:     map[int64][]source.Candidate{},
		blockTime: func(n int64) time.Time { return t0.Add(time.Duration(n) * time.Second) },
	}}
	e.exec(e.led, `INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount)
		VALUES ($1, 'erc20', $2, 'TKN', 6, 0, 0, 0)`, assetID, token)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, user_id, total_balance, locked_balance) VALUES ('bbbbbbbb-0000-0000-0000-000000000001', $1, 'user', 'u1', 500, 0)`, assetID)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, total_balance) VALUES ('bbbbbbbb-0000-0000-0000-000000000002', $1, 'chain', -500)`, assetID)
	e.exec(e.led, `INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash, chain_id, tx_hash, log_index, created_at)
		VALUES ($1, 'deposit', $2, 'h', $3, $4, $5, $6)`, settle, dRef, dChain, dTx, dLog, t0)
	e.exec(e.led, `INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES
		($1, 'bbbbbbbb-0000-0000-0000-000000000001', 500), ($1, 'bbbbbbbb-0000-0000-0000-000000000002', -500)`, settle)

	e.exec(e.wal, `INSERT INTO wallet_addresses (user_id, derivation_index, address) VALUES ('u1', 1, $1)`, depAdr)
	e.exec(e.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount,
		block_number, block_hash, status, detected_at, confirmed_at) VALUES ($1, $2, $3, $4, '0x00000000000000000000000000000000000000f1', $5, 'u1', 500, $6, $7, 'confirmed', $8, $8)`,
		dChain, dTx, dLog, assetID, depAdr, dBlock, dHash, t0)
	e.exec(e.cw, `INSERT INTO watermark (chain_id, block_number) VALUES ($1, 100)`, dChain)

	e.hist["deposit:"+dRef] = bson.M{"status": "confirmed", "tx_hash": dTx, "amount": "500", "asset_id": assetID}
	d.chain.transfers[dRef] = deposit.OnChain{Found: true, Success: true, Block: dBlock, BlockHash: dHash, Canonical: dHash,
		To: depAdr, Amount: big.NewInt(500), Token: token, AssetID: ""}
	d.chain.cands[dBlock] = []source.Candidate{{TxHash: dTx, LogIndex: dLog, To: depAdr, Amount: big.NewInt(500)}}

	d.job = &DepositAudit{
		Src:   &source.Deposits{Watcher: e.ro["chain-watcher"], Ledger: e.ro["ledger"], Wallet: e.ro["wallet"], History: e.hist, Chain: d.chain},
		Store: e.st, ChainID: dChain, Every: 5 * time.Minute, Grace: 10 * time.Minute, Escalate: 15 * time.Minute,
		Batch: 2, ScanBlocks: 40, Now: func() time.Time { return e.now },
	}
	return d
}

func (d *depEnv) run() job.Result {
	d.t.Helper()
	res, err := job.RunOnce(context.Background(), d.st, d.job, func() time.Time { return d.now })
	if err != nil {
		d.t.Fatal(err)
	}
	d.now = d.now.Add(5 * time.Minute)
	return res
}

func (d *depEnv) findingsOf() map[string]found {
	d.t.Helper()
	rows, err := d.st.Findings(context.Background(), "deposit")
	if err != nil {
		d.t.Fatal(err)
	}
	out := map[string]found{}
	for _, r := range rows {
		out[r.Rule] = found{r.Rule, r.Status, r.Severity}
	}
	return out
}

func TestConsistentDepositIsOk(t *testing.T) {
	d := newDepEnv(t)
	for i := 0; i < 3; i++ {
		if res := d.run(); res != job.OK {
			t.Fatalf("run %d: %s %v", i+1, res, d.findingsOf())
		}
	}
	if f := d.findingsOf(); len(f) != 0 {
		t.Fatalf("findings: %v", f)
	}
}

func TestDepositMismatchNeedsTwoRuns(t *testing.T) {
	d := newDepEnv(t)
	d.exec(d.wal, `UPDATE wallet_addresses SET user_id = 'u2'`) // the credited user is not the address owner
	if res := d.run(); res != job.OK {
		t.Fatalf("first sighting: %s", res)
	}
	if res := d.run(); res != job.Fail {
		t.Fatalf("second run: %s", res)
	}
	if f := d.findingsOf()["D3"]; f.status != "open" {
		t.Fatalf("D3: %+v", f)
	}
}

func TestNotCreditedNorFlagged(t *testing.T) {
	d := newDepEnv(t)
	// entries are append-only, so make the credit belong to a different transfer instead
	d.exec(d.led, `UPDATE ledger_transactions SET log_index = 99 WHERE source_type = 'deposit'`)
	d.run()
	d.run()
	if f := d.findingsOf()["D1"]; f.status != "open" {
		t.Fatalf("D1: %+v", f)
	}
}

func TestCreditWithoutObservedTransfer(t *testing.T) {
	d := newDepEnv(t)
	d.exec(d.cw, `DELETE FROM observed_transfers`)
	d.run()
	d.run()
	f := d.findingsOf()
	if f["D2"].status != "open" {
		t.Fatalf("D2: %v", f)
	}
}

func TestReorgedBlockIsD5(t *testing.T) {
	d := newDepEnv(t)
	c := d.chain.transfers[dRef]
	c.Canonical = "0xf0000000000000000000000000000000000000000000000000000000000000f0"
	d.chain.transfers[dRef] = c
	d.run()
	d.run()
	if f := d.findingsOf()["D5"]; f.status != "open" || f.severity != "critical" {
		t.Fatalf("D5: %+v", f)
	}
}

func TestLaggingHistoryWarnsThenEscalates(t *testing.T) {
	d := newDepEnv(t)
	delete(d.hist, "deposit:"+dRef)
	d.run()
	if res := d.run(); res != job.Warn {
		t.Fatalf("D6 should warn: %s", res)
	}
	d.now = d.now.Add(30 * time.Minute)
	if res := d.run(); res != job.Fail {
		t.Fatalf("D6 should have escalated: %s", res)
	}
}

// D7: a transfer to a deposit address chain-watcher has no record of.
func TestMissedByTheWatcher(t *testing.T) {
	d := newDepEnv(t)
	missed := "0xdee0000000000000000000000000000000000000000000000000000000000002"
	d.chain.cands[20] = []source.Candidate{{TxHash: missed, LogIndex: 1, To: depAdr, Amount: big.NewInt(77)}}
	d.chain.transfers[source.DepositKey(dChain, missed, 1)] = deposit.OnChain{Found: true, Success: true, Block: 20,
		BlockHash: "0x20", Canonical: "0x20", To: depAdr, Amount: big.NewInt(77), Token: token}
	d.run()
	if res := d.run(); res != job.Fail {
		t.Fatalf("a missed deposit: %s %v", res, d.findingsOf())
	}
	if f := d.findingsOf()["D7"]; f.status != "open" {
		t.Fatalf("D7: %+v", f)
	}
	// the watcher catches up: resolved
	d.exec(d.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount,
		block_number, block_hash, status) VALUES ($1, $2, 1, $3, '0x00000000000000000000000000000000000000f1', $4, 'u1', 77, 20, '0x20', 'pending')`,
		dChain, missed, assetID, depAdr)
	if res := d.run(); res == job.Fail {
		t.Fatalf("after the watcher caught up: %s %v", res, d.findingsOf())
	}
	if f := d.findingsOf()["D7"]; f.status != "resolved" {
		t.Fatalf("D7 should resolve: %+v", f)
	}
}

// The scan stops at the watermark and at the Grace Period, in batches, and resumes where it stopped.
func TestScanRespectsWatermarkAndGrace(t *testing.T) {
	d := newDepEnv(t)
	// only blocks up to 55 are older than the Grace Period at the first run (now = t0+1h, grace 10m)
	d.chain.blockTime = func(n int64) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	d.run()
	cur, _ := d.st.Cursor(context.Background(), cursorBlocks)
	if cur == "" || cur == "100" {
		t.Fatalf("the scan must stop short of the newest blocks: cursor %q", cur)
	}
	for _, s := range d.chain.scans {
		if s[1]-s[0]+1 > 40 {
			t.Errorf("batch %v is larger than ScanBlocks", s)
		}
		if s[1] > 100 {
			t.Errorf("scanned past the watermark: %v", s)
		}
	}
	// much later everything is old enough: it resumes and reaches the watermark
	d.now = d.now.Add(10 * time.Hour)
	before := len(d.chain.scans)
	d.run()
	if cur, _ := d.st.Cursor(context.Background(), cursorBlocks); cur != "100" {
		t.Fatalf("the scan should reach the watermark, cursor %q", cur)
	}
	if d.chain.scans[before][0] <= 0 {
		t.Errorf("did not resume from the cursor: %v", d.chain.scans[before:])
	}
}
