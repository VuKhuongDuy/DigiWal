package audit

import (
	"context"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/withdrawal"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
	"digiwal/services/auditor/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	wid     = "11111111-1111-1111-1111-111111111111"
	assetID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	token   = "0x00000000000000000000000000000000000000c0"
	dest    = "0x00000000000000000000000000000000000000d1"
	txHash  = "0xabc0000000000000000000000000000000000000000000000000000000000001"
	settle  = "22222222-2222-2222-2222-222222222222"
)

// fakeHistory and fakeChain stand in for MongoDB and the RPC.
type fakeHistory map[string]bson.M

func (h fakeHistory) FindOne(_ context.Context, _ string, id any) (bson.M, error) {
	if d, ok := h[id.(string)]; ok {
		return d, nil
	}
	return nil, mongo.ErrNoDocuments
}

type fakeChain map[string]withdrawal.ChainTx

func (c fakeChain) Tx(_ context.Context, h string) (withdrawal.ChainTx, error) {
	return c[strings.ToLower(h)], nil
}

type env struct {
	t            *testing.T
	led, wal, wd *pgxpool.Pool // superuser, to seed and to tamper
	st           *store.Store
	hist         fakeHistory
	chain        fakeChain
	now          time.Time
	auditJob     *WithdrawalAudit
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testdb.Admin(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, readFile(t, "../../roles/00_role.sql")); err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, hist: fakeHistory{}, chain: fakeChain{}, now: t0.Add(time.Hour)}
	var ro = map[string]source.Querier{}
	pools := map[string]*pgxpool.Pool{}
	for _, st := range source.Stores {
		if st.Name != "ledger" && st.Name != "wallet" && st.Name != "withdrawal" {
			continue
		}
		dsn := testdb.New(t)
		testdb.ApplyMigrations(t, dsn, filepath.Join("..", "..", st.MigrationsDir))
		testdb.ApplyFile(t, dsn, filepath.Join("..", "..", st.RolesFile))
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		pools[st.Name] = p
		// the auditor reads as auditor_ro, so a column missing from Contract/roles fails here
		pg, err := source.OpenPG(ctx, st.Name, testdb.WithUser(t, dsn, testdb.DBName(t, dsn), "auditor_ro", "auditor_ro"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pg.Close)
		ro[st.Name] = pg
	}
	e.led, e.wal, e.wd = pools["ledger"], pools["wallet"], pools["withdrawal"]
	e.st, err = store.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.st.Close)

	e.auditJob = &WithdrawalAudit{
		Src:   &source.Withdrawals{Withdrawal: ro["withdrawal"], Ledger: ro["ledger"], Wallet: ro["wallet"], History: e.hist, Chain: e.chain},
		Store: e.st, Every: 5 * time.Minute, Grace: 10 * time.Minute, Escalate: 15 * time.Minute, Batch: 2,
		Now: func() time.Time { return e.now },
	}
	return e
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) exec(p *pgxpool.Pool, sql string, args ...any) {
	e.t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%v\n%s", err, sql)
	}
}

func transferInput(to string, amount int64) []byte {
	b, _ := hex.DecodeString("a9059cbb")
	b = append(b, make([]byte, 12)...)
	a, _ := hex.DecodeString(strings.TrimPrefix(to, "0x"))
	b = append(b, a...)
	amt := make([]byte, 32)
	big.NewInt(amount).FillBytes(amt)
	return append(b, amt...)
}

// seedConfirmed writes one fully consistent confirmed ERC-20 withdrawal of 1000 into every store.
func (e *env) seedConfirmed() {
	e.exec(e.led, `INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount)
		VALUES ($1, 'erc20', $2, 'TKN', 6, 0, 0, 0)`, assetID, token)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, user_id, total_balance, locked_balance) VALUES
		('bbbbbbbb-0000-0000-0000-000000000001', $1, 'user', 'u1', 0, 0)`, assetID)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, total_balance) VALUES ('bbbbbbbb-0000-0000-0000-000000000002', $1, 'chain', 1000)`, assetID)
	e.exec(e.led, `INSERT INTO locks (idempotency_key, account_id, amount, status, resolved_at)
		VALUES ('withdrawal:`+wid+`', 'bbbbbbbb-0000-0000-0000-000000000001', 1000, 'settled', now())`)
	e.exec(e.led, `INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash) VALUES ($1, 'withdrawal_settle', 'withdrawal:`+wid+`', 'h')`, settle)
	e.exec(e.led, `INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES
		($1, 'bbbbbbbb-0000-0000-0000-000000000001', -1000), ($1, 'bbbbbbbb-0000-0000-0000-000000000002', 1000)`, settle)
	e.exec(e.led, `INSERT INTO ledger_transactions (source_type, idempotency_key, request_hash, purpose)
		VALUES ('gas_fee', 'gas:31337:`+txHash+`', 'g', 'withdrawal')`)

	e.exec(e.wal, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, nonce, request_hash, signed_raw_tx, caller_cn)
		VALUES ('withdrawal:`+wid+`', 31337, 7, 'h', '\x00', 'withdrawal-service')`)

	e.exec(e.wd, `INSERT INTO withdrawal_requests (withdrawal_id, user_id, idempotency_key, asset_id, amount, to_address, status, tx_hash,
		nonce, signed_raw_tx, chain_id, broadcast_at, confirmed_block, settle_ledger_tx_id, confirmed_at, updated_at, gas_price)
		VALUES ($1, 'u1', 'k1', $2, 1000, $3, 'confirmed', $4, 7, '0x00', 31337, $5, 50, $6, $5, $5, 1000000000)`,
		wid, assetID, dest, txHash, t0, settle)

	e.hist["withdrawal:"+wid] = bson.M{"status": "confirmed", "tx_hash": txHash, "amount": "1000", "asset_id": assetID}
	e.chain[txHash] = withdrawal.ChainTx{Found: true, Mined: true, Success: true, Block: 50, To: token, Value: big.NewInt(0), Input: transferInput(dest, 1000)}
}

func (e *env) run() job.Result {
	e.t.Helper()
	res, err := job.RunOnce(context.Background(), e.st, e.auditJob, func() time.Time { return e.now })
	if err != nil {
		e.t.Fatal(err)
	}
	e.now = e.now.Add(5 * time.Minute)
	return res
}

type found struct{ rule, status, severity string }

func (e *env) findings() map[string]found {
	e.t.Helper()
	rows, err := e.st.Findings(context.Background(), finding.FlowWithdrawal)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]found{}
	for _, r := range rows {
		out[r.Rule] = found{r.Rule, r.Status, r.Severity}
	}
	return out
}

func TestConsistentWithdrawalIsOk(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	for i := 0; i < 3; i++ {
		if res := e.run(); res != job.OK {
			t.Fatalf("run %d: %s", i+1, res)
		}
	}
	if f := e.findings(); len(f) != 0 {
		t.Fatalf("findings on a consistent withdrawal: %v", f)
	}
}

func TestNothingIsAuditedBeforeTheGracePeriod(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	e.exec(e.wd, `UPDATE withdrawal_requests SET settle_ledger_tx_id = 'broken', updated_at = $1`, e.now.Add(-time.Minute))
	e.run()
	e.run()
	if f := e.findings(); len(f) != 0 {
		t.Fatalf("a request final for less than the Grace Period must not be audited: %v", f)
	}
}

func TestMismatchBecomesAFindingOnTheSecondRun(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	e.exec(e.wd, `UPDATE withdrawal_requests SET settle_ledger_tx_id = 'not-the-settle'`)

	if res := e.run(); res != job.OK || len(e.findings()) != 0 {
		t.Fatalf("first sighting must not be a finding yet: %s %v", res, e.findings())
	}
	if res := e.run(); res != job.Fail {
		t.Fatalf("second run: %s, want fail", res)
	}
	if f := e.findings()["W1"]; f.status != "open" || f.severity != "critical" {
		t.Fatalf("W1: %+v", f)
	}

	// fixed: it resolves, and the row stays (never deleted)
	e.exec(e.wd, `UPDATE withdrawal_requests SET settle_ledger_tx_id = $1`, settle)
	if res := e.run(); res != job.OK {
		t.Fatalf("after the fix: %s", res)
	}
	if f := e.findings()["W1"]; f.status != "resolved" {
		t.Fatalf("W1 after the fix: %+v", f)
	}
}

// A history-service that is merely behind is a warn, and becomes critical if it stays behind.
func TestLaggingReadModelWarnsThenEscalates(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	delete(e.hist, "withdrawal:"+wid)
	e.run()
	if res := e.run(); res != job.Warn {
		t.Fatalf("W5 should warn: %s", res)
	}
	if f := e.findings()["W5"]; f.severity != "warn" {
		t.Fatalf("W5: %+v", f)
	}
	e.now = e.now.Add(30 * time.Minute)
	if res := e.run(); res != job.Fail {
		t.Fatalf("W5 should have escalated: %s", res)
	}
	// history catches up
	e.hist["withdrawal:"+wid] = bson.M{"status": "confirmed", "tx_hash": txHash, "amount": "1000", "asset_id": assetID}
	if res := e.run(); res != job.OK {
		t.Fatalf("after catching up: %s", res)
	}
}

// A signature made for another caller cannot be a timing artefact: recorded in one run.
func TestSigningFindingIsImmediate(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	e.exec(e.wal, `UPDATE hot_wallet_signatures SET caller_cn = 'sweeper'`)
	if res := e.run(); res != job.Fail {
		t.Fatalf("run 1: %s", res)
	}
	if f := e.findings()["W6"]; f.status != "open" {
		t.Fatalf("W6: %+v", f)
	}
}

func TestChainDisagreesWithRequest(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	c := e.chain[txHash]
	c.Input = transferInput("0x00000000000000000000000000000000000000e2", 1000)
	e.chain[txHash] = c
	e.run()
	e.run()
	if f := e.findings()["W7"]; f.status != "open" || f.severity != "critical" {
		t.Fatalf("W7: %+v", f)
	}
}

func TestOrphanSettle(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	e.exec(e.wd, `DELETE FROM withdrawal_requests`)
	e.run()
	if res := e.run(); res != job.Fail {
		t.Fatalf("a Settle with no request: %s", res)
	}
	if f := e.findings()["W2"]; f.status != "open" {
		t.Fatalf("W2: %+v", f)
	}
}

// Rows are visited in batches (Batch is 2 here); every request is audited, none twice per run.
func TestScansEveryRequestAcrossBatches(t *testing.T) {
	e := newEnv(t)
	e.seedConfirmed()
	for i := 0; i < 5; i++ {
		e.exec(e.wd, `INSERT INTO withdrawal_requests (user_id, idempotency_key, asset_id, amount, to_address, status, reject_reason, updated_at)
			VALUES ('u2', $1, $2, 5, $3, 'rejected', 'not_whitelisted', $4)`, "r"+string(rune('a'+i)), assetID, dest, t0.Add(time.Duration(i)*time.Second))
		// a rejected request that nonetheless holds a Lock is W4
	}
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, user_id, total_balance, locked_balance) VALUES ('bbbbbbbb-0000-0000-0000-000000000009', $1, 'user', 'u2', 100, 5)`, assetID)
	var rid string
	if err := e.wd.QueryRow(context.Background(), `SELECT withdrawal_id::text FROM withdrawal_requests WHERE idempotency_key = 'rc'`).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	e.exec(e.led, `INSERT INTO locks (idempotency_key, account_id, amount, status) VALUES ('withdrawal:`+rid+`', 'bbbbbbbb-0000-0000-0000-000000000009', 5, 'active')`)
	e.run()
	e.run()
	rows, _ := e.st.Findings(context.Background(), finding.FlowWithdrawal)
	if len(rows) != 1 || rows[0].Rule != "W4" {
		t.Fatalf("exactly the one rejected request with an active Lock should be found, got %+v", rows)
	}
	if cur, _ := e.st.Cursor(context.Background(), cursorRequests); cur == "" {
		t.Error("the cursor should have advanced")
	}
}
