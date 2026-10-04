package e2e

import (
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func ledgerUp() bool { return do(nil2(), "GET", ledgerHTP+"/healthz", nil).Code == 200 }
func waitLedger(t testing.TB) {
	waitFor(t, "ledger healthy", 60*time.Second, ledgerUp)
}

// retry until the operation reaches a final answer (200 or 409/404/422), riding out an outage.
func persist(f func() resp) resp {
	for i := 0; i < 400; i++ {
		r := f()
		if r.Code == 200 || r.Code == 409 || r.Code == 404 || r.Code == 422 {
			return r
		}
		time.Sleep(150 * time.Millisecond)
	}
	return resp{Code: -2}
}

// OPS-02: kill -9 the ledger in the middle of a Settle storm; every lock must end settled exactly once.
func TestOPS02_KillLedgerDuringSettleStorm(t *testing.T) {
	qa(t)
	u := uid("ops02")
	credit(t, u, ethAsset, 6000)
	ws := make([]*wd, 60)
	for i := range ws {
		ws[i] = newWd(u, ethAsset, 100)
		if r := ws[i].lock(); r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func(w *wd) { defer wg.Done(); persist(w.settle) }(w)
	}
	time.Sleep(150 * time.Millisecond)
	compose(t, "kill", "ledger-service")
	time.Sleep(2 * time.Second)
	compose(t, "start", "ledger-service")
	wg.Wait()
	waitLedger(t)
	wantBal(t, u, ethAsset, 0, 0)
	if n := count(t, `SELECT count(*) FROM locks WHERE account_id=(SELECT id FROM accounts WHERE user_id=$1 AND asset_id=$2) AND status='settled'`, u, ethAsset); n != 60 {
		t.Fatalf("%d of 60 locks settled", n)
	}
	if n := count(t, `SELECT count(*) FROM ledger_transactions WHERE source_type='withdrawal_settle' AND idempotency_key = ANY($1)`, keys(ws)); n != 60 {
		t.Fatalf("%d settle txs for 60 locks", n)
	}
}

func keys(ws []*wd) []string {
	k := make([]string, len(ws))
	for i, w := range ws {
		k[i] = w.key
	}
	return k
}

// DEP-04/05: kill the consumer repeatedly while a stream of deposits flows. Each deposit credited exactly once.
func TestDEP04_05_ConsumerKilledRepeatedly(t *testing.T) {
	qa(t)
	u := uid("dep04")
	const total = 150
	var evs []depositEvent
	for i := 0; i < total; i++ {
		evs = append(evs, depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(fmt.Sprint(u, i)))).Hex(), -1, ethAsset,
			"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "7", 5})
	}
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(1200 * time.Millisecond)
			dkr(t, "kill", "digiwal-qa-ledger-service-1")
			time.Sleep(500 * time.Millisecond)
			dkr(t, "start", "digiwal-qa-ledger-service-1")
		}
	}()
	for i := 0; i < total; i += 10 {
		publish(t, evs[i:i+10]...)
		time.Sleep(150 * time.Millisecond)
	}
	waitFor(t, "all credited", 120*time.Second, func() bool { return balance(t, u, ethAsset).Total.Int64() >= total*7 })
	time.Sleep(5 * time.Second)
	wantBal(t, u, ethAsset, total*7, 0)
	if n := count(t, `SELECT count(*) FROM ledger_transactions WHERE source_type='deposit' AND idempotency_key LIKE '31337:%' AND id IN (SELECT ledger_transaction_id FROM entries e JOIN accounts a ON a.id=e.account_id WHERE a.user_id=$1)`, u); n != total {
		t.Fatalf("%d ledger txs for %d events", n, total)
	}
}

func dkr(t testing.TB, args ...string) {
	t.Helper()
	if out, err := execCmd(args...); err != nil {
		t.Logf("docker %v: %v %s", args, err, out)
	}
}

// DEP-06: Postgres restarted while deposits are being processed. Events must be retried, not parked, never lost.
func TestDEP06_PostgresRestartDuringDeposits(t *testing.T) {
	qa(t)
	u := uid("dep06")
	const total = 40
	failedBefore := count(t, `SELECT count(*) FROM failed_events`)
	var evs []depositEvent
	for i := 0; i < total; i++ {
		evs = append(evs, depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(fmt.Sprint(u, i)))).Hex(), -1, ethAsset,
			"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "3", 5})
	}
	publish(t, evs[:20]...)
	go func() { time.Sleep(300 * time.Millisecond); dkr(t, "restart", "digiwal-qa-postgres-1") }()
	time.Sleep(500 * time.Millisecond)
	publish(t, evs[20:]...)
	waitFor(t, "postgres back", 90*time.Second, func() bool { _, err := ldb.Exec(ctxBG(), "select 1"); return err == nil })
	waitFor(t, "all credited after outage", 120*time.Second, func() bool { return balance(t, u, ethAsset).Total.Int64() == total*3 })
	if fa := count(t, `SELECT count(*) FROM failed_events`); fa != failedBefore {
		t.Errorf("FINDING: %d deposits were PARKED as 'unprocessable' during a DB outage (they were transient)", fa-failedBefore)
	}
	wantBal(t, u, ethAsset, total*3, 0)
}

// K-1: ledger-service boots before deposits.confirmed exists (fresh environment / chain-watcher not started yet).
func TestOPS_K1_ConsumerBootsBeforeTopicExists(t *testing.T) {
	qa(t)
	dkr(t, "stop", "digiwal-qa-ledger-service-1")
	dkr(t, "exec", "digiwal-qa-kafka-1", "/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:9092", "--delete", "--topic", "deposits.confirmed")
	time.Sleep(3 * time.Second)
	dkr(t, "start", "digiwal-qa-ledger-service-1")
	waitLedger(t)
	time.Sleep(8 * time.Second)
	u := uid("k1")
	want := balance(t, u, ethAsset).Total.Int64() + 55
	e := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(u))).Hex(), -1, ethAsset,
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "55", 5}
	publish(t, e) // first producer creates the topic
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if balance(t, u, ethAsset).Total.Int64() == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("FINDING K-1 (HIGH, silent stuck system): ledger-service started while deposits.confirmed did not exist; after the topic was created and a deposit published, nothing was consumed for 90s (healthz is green, no error logged). kafka-go's reader needs WatchPartitionChanges / a topic-ensure step; until restart no deposit is credited.")
	dkr(t, "restart", "digiwal-qa-ledger-service-1") // recover so later tests work
	waitLedger(t)
	waitCredited(t, u, ethAsset, want)
}

// SEC-08: no read timeouts anywhere: a client that trickles bytes holds a connection forever.
func TestSEC08_NoHeaderTimeouts(t *testing.T) {
	qa(t)
	for name, addr := range map[string]string{"wallet plain": "localhost:18080", "ledger plain": "localhost:18081", "wallet mTLS (no handshake)": "localhost:18443", "ledger mTLS (no handshake)": "localhost:18444"} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(name, "mTLS") {
			fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Slow: ")
		}
		c.SetReadDeadline(time.Now().Add(25 * time.Second))
		buf := make([]byte, 1)
		_, rerr := c.Read(buf)
		if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
			t.Errorf("FINDING S-1 (MEDIUM): %s listener kept a half-open connection for 25s with no data and never closed it (no ReadHeaderTimeout/ReadTimeout set): slowloris / fd exhaustion", name)
		}
		c.Close()
	}
}

// OPS-05: RPC node hangs. The signing endpoint must answer (error) in bounded time and leave no state behind.
func TestOPS05_RPCStallDoesNotHangSigning(t *testing.T) {
	qa(t)
	before := dbNonce(t)
	dkr(t, "pause", "digiwal-qa-anvil-1")
	done := make(chan resp, 1)
	go func() { done <- signHot(uid("stall"), randAddr().Hex(), ethAsset, "1000") }()
	var r resp
	select {
	case r = <-done:
	case <-time.After(40 * time.Second):
		r = resp{Code: -3}
	}
	dkr(t, "unpause", "digiwal-qa-anvil-1")
	waitFor(t, "anvil back", 30*time.Second, func() bool { _, err := cl.BlockNumber(ctxBG()); return err == nil })
	if r.Code == -3 || r.Code == -1 {
		t.Errorf("FINDING W-8 (MEDIUM/HIGH): sign endpoint did not answer within 40s while the RPC node was unresponsive (code %d): no timeout on RPC calls, handler goroutines pile up behind a dead node", r.Code)
	}
	if dbNonce(t) != before {
		t.Errorf("stalled request moved the nonce: %d -> %d", before, dbNonce(t))
	}
	// service still alive afterwards
	if s := signHot(uid("after"), randAddr().Hex(), ethAsset, "1000"); s.Code != 200 {
		t.Errorf("signing broken after RPC recovered: %d %s", s.Code, s.Body)
	}
}

// LED-02: randomised concurrent workload on the ledger API; invariants must hold at the end.
func TestLED02_RandomWorkloadKeepsInvariants(t *testing.T) {
	qa(t)
	users := []string{uid("led-a"), uid("led-b"), uid("led-c"), uid("led-d"), uid("led-e")}
	for _, u := range users {
		credit(t, u, ethAsset, 50_000)
		credit(t, u, ttAsset, 50_000)
	}
	var mu sync.Mutex
	var live []*wd
	var ops atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for i := 0; i < 40; i++ {
				ops.Add(1)
				switch p := rnd.Intn(10); {
				case p < 4: // new lock, sometimes oversized
					asset := []string{ethAsset, ttAsset}[rnd.Intn(2)]
					w := newWd(users[rnd.Intn(5)], asset, int64(1+rnd.Intn(30_000)))
					if r := w.lock(); r.Code == 200 {
						mu.Lock()
						live = append(live, w)
						mu.Unlock()
					}
				case p < 6: // settle something (maybe again)
					mu.Lock()
					if len(live) > 0 {
						w := live[rnd.Intn(len(live))]
						mu.Unlock()
						w.settle()
					} else {
						mu.Unlock()
					}
				case p < 8: // release something
					mu.Lock()
					if len(live) > 0 {
						w := live[rnd.Intn(len(live))]
						mu.Unlock()
						ledgerRelease(w.key)
					} else {
						mu.Unlock()
					}
				case p < 9: // replay a lock
					mu.Lock()
					if len(live) > 0 {
						w := live[rnd.Intn(len(live))]
						mu.Unlock()
						w.lock()
					} else {
						mu.Unlock()
					}
				default: // correction of a settled withdrawal
					mu.Lock()
					var w *wd
					if len(live) > 0 {
						w = live[rnd.Intn(len(live))]
					}
					mu.Unlock()
					if w != nil {
						correctSettled(w.key)
					}
				}
			}
		}(int64(g))
	}
	wg.Wait()
	t.Logf("%d operations, %d locks created", ops.Load(), len(live))
	for _, u := range users {
		for _, a := range []string{ethAsset, ttAsset} {
			b := balance(t, u, a)
			if b.Avail.Sign() < 0 {
				t.Errorf("%s %s available negative: %s", u, a, b.Avail)
			}
		}
	}
}

func correctSettled(key string) {
	rows, err := ldb.Query(ctxBG(), `SELECT e.id FROM entries e JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1 AND l.source_type='withdrawal_settle'`, key)
	if err != nil {
		return
	}
	var ids []map[string]string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, map[string]string{"corrects_entry_id": id})
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	do(client("withdrawal-service"), "POST", ledgerTLS+"/v1/ledger/transactions", map[string]any{
		"idempotency_key": key + ":refund", "source_type": "correction", "entries": ids})
}
