package chaos

import (
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type batch struct {
	u     *user
	keys  []string
	dests []common.Address
	amt   *big.Int
}

// newBatch gives a user `funds` ETH in the ledger and n whitelisted destinations.
func newBatch(t *testing.T, tag string, funds *big.Int, n int, amt *big.Int) *batch {
	t.Helper()
	u := newUser(t)
	u.fundAndCredit(t, ethAsset, funds)
	b := &batch{u: u, amt: amt}
	for i := 0; i < n; i++ {
		d := fresh()
		b.dests = append(b.dests, d)
		b.keys = append(b.keys, fmt.Sprintf("%s-%s-%d", tag, u.id[:8], i))
	}
	var wg sync.WaitGroup
	for _, d := range b.dests {
		wg.Add(1)
		go func(d common.Address) { defer wg.Done(); do("POST", "/v1/whitelist", u.key, map[string]string{"address": d.Hex()}) }(d)
	}
	wg.Wait()
	time.Sleep(3 * time.Second) // cooldown 2s
	return b
}

func (b *batch) submitAll() map[int]int {
	codes := make([]int, len(b.keys))
	var wg sync.WaitGroup
	for i := range b.keys {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], _ = b.u.withdraw(b.keys[i], ethAsset, b.amt, b.dests[i]) }(i)
	}
	wg.Wait()
	m := map[int]int{}
	for _, c := range codes {
		m[c]++
	}
	return m
}

// verify: every destination holds 0 or exactly amt; confirmed <=> paid; ledger debit == paid total; nothing left locked.
func (b *batch) verify(t *testing.T, funds *big.Int, status map[string]string) (paid int) {
	t.Helper()
	for i, k := range b.keys {
		got := ethBal(b.dests[i])
		s := status[k]
		switch {
		case got.Sign() == 0:
			if s == "confirmed" {
				t.Errorf("MONEY LOST? %s is 'confirmed' but destination %s received nothing", k, b.dests[i])
			}
		case got.Cmp(b.amt) == 0:
			paid++
			if s != "confirmed" {
				t.Errorf("PAID BUT NOT CONFIRMED: %s status %q yet destination holds %s", k, s, got)
			}
		default:
			t.Errorf("DOUBLE/PARTIAL PAYOUT: %s destination %s holds %s (expected 0 or %s)", k, b.dests[i], got, b.amt)
		}
	}
	bl := b.u.bal(t, ethAsset)
	wantTotal := new(big.Int).Sub(funds, new(big.Int).Mul(big.NewInt(int64(paid)), b.amt))
	if bl.Total.Cmp(wantTotal) != 0 {
		t.Errorf("LEDGER/CHAIN MISMATCH: paid %d x %s on chain, ledger total %s, want %s", paid, b.amt, bl.Total, wantTotal)
	}
	if bl.Locked.Sign() != 0 {
		t.Errorf("funds still locked after all terminal: %s", bl.Locked)
	}
	return paid
}

// C3: the same withdrawal submitted 20x at once.
func TestC3_DoubleSubmitPaysOnce(t *testing.T) {
	qa(t)
	b := newBatch(t, "c3", eth1(2), 1, eth1(1))
	codes := make([]int, 20)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], _ = b.u.withdraw(b.keys[0], ethAsset, b.amt, b.dests[0]) }(i)
	}
	wg.Wait()
	m := map[int]int{}
	for _, c := range codes {
		m[c]++
	}
	t.Logf("20 identical submissions -> %v", m)
	for c := range m {
		if c >= 500 || c == -1 {
			t.Errorf("server error %d on duplicate submit", c)
		}
	}
	if c, _ := b.u.withdraw(b.keys[0], ethAsset, eth1(2), b.dests[0]); c != 409 {
		t.Errorf("same key, different amount -> %d, want 409", c)
	}
	st := b.u.settleAll(t, b.keys, 120*time.Second)
	b.verify(t, eth1(2), st)
	if n := psql(t, "withdrawal", fmt.Sprintf(`SELECT count(*) FROM withdrawal_requests WHERE user_id='%s'`, b.u.id)); n != "1" {
		t.Errorf("%s withdrawal rows for one key", n)
	}
}

// C4: 20 racing withdrawals of 0.1 ETH against exactly 1 ETH.
func TestC4_RacingWithdrawalsNeverOverdraw(t *testing.T) {
	qa(t)
	b := newBatch(t, "c4", eth1(1), 20, finney(100))
	codes := b.submitAll()
	t.Logf("20 x 0.1 ETH against 1 ETH -> %v", codes)
	accepted := codes[200] + codes[201] + codes[202]
	if accepted != 10 {
		t.Errorf("accepted %d, want exactly 10", accepted)
	}
	st := b.u.settleAll(t, b.keys, 150*time.Second)
	paid := b.verify(t, eth1(1), st)
	if paid != 10 {
		t.Errorf("paid %d of 10 accepted", paid)
	}
	if bl := b.u.bal(t, ethAsset); bl.Total.Sign() < 0 || bl.Avail.Sign() < 0 {
		t.Errorf("NEGATIVE BALANCE %s/%s", bl.Total, bl.Avail)
	}
}

func killStart(t *testing.T, svc string, down time.Duration) {
	t.Logf(">> docker kill %s (SIGKILL)", ctr(svc))
	dc(t, "kill", ctr(svc))
	time.Sleep(down)
	t.Logf(">> docker start %s", ctr(svc))
	dc(t, "start", ctr(svc))
}

func chaosWithdrawals(t *testing.T, tag string, fault func()) {
	qa(t)
	b := newBatch(t, tag, eth1(3), 8, finney(200))
	t.Logf("submitting 8 withdrawals of 0.2 ETH; fault injected ~0.4s later")
	done := make(chan struct{})
	go func() { time.Sleep(400 * time.Millisecond); fault(); close(done) }()
	t.Logf("codes: %v", b.submitAll())
	<-done
	st := b.u.settleAll(t, b.keys, 240*time.Second)
	t.Logf("final states: %v", st)
	for k, s := range st {
		if !terminal(s) {
			t.Errorf("STUCK: %s still %q after recovery", k, s)
		}
	}
	paid := b.verify(t, eth1(3), st)
	t.Logf("paid on chain: %d of 8", paid)
}

// C5: kill -9 withdrawal-service while the pipeline is mid-flight.
func TestC5_KillWithdrawalService(t *testing.T) {
	chaosWithdrawals(t, "c5", func() { killStart(t, "withdrawal-service", 3*time.Second) })
}

// C7: kill -9 ledger-service (Lock/Settle target) mid-flight.
func TestC7_KillLedgerService(t *testing.T) {
	chaosWithdrawals(t, "c7", func() { killStart(t, "ledger-service", 4*time.Second) })
}

// C8: Postgres (shared by every service) restarts mid-flight.
func TestC8_PostgresRestart(t *testing.T) {
	chaosWithdrawals(t, "c8", func() {
		t.Logf(">> docker restart %s", ctr("postgres"))
		dc(t, "restart", ctr("postgres"))
		time.Sleep(5 * time.Second)
	})
}

// C9: Kafka restarts mid-flight (events between services).
func TestC9_KafkaRestart(t *testing.T) {
	chaosWithdrawals(t, "c9", func() {
		t.Logf(">> docker restart %s", ctr("kafka"))
		dc(t, "restart", ctr("kafka"))
		time.Sleep(5 * time.Second)
	})
}

// C10: the RPC node hangs (pause) for 25 s while withdrawals are in flight.
func TestC10_RPCNodeHangs(t *testing.T) {
	chaosWithdrawals(t, "c10", func() {
		t.Logf(">> docker pause %s (25s)", ctr("anvil"))
		dc(t, "pause", ctr("anvil"))
		time.Sleep(25 * time.Second)
		dc(t, "unpause", ctr("anvil"))
		t.Logf(">> docker unpause %s", ctr("anvil"))
	})
}

// C6: kill chain-watcher in the middle of a deposit stream. Total credited must equal total sent, exactly.
func TestC6_KillChainWatcherDuringDeposits(t *testing.T) {
	qa(t)
	u := newUser(t)
	const n = 8
	for i := 0; i < n; i++ {
		if _, err := sendRaw(ctxBG(), key1, &u.dep, finney(100), nil); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			go killStart(t, "chain-watcher", 3*time.Second)
		}
		mine(t, 1)
		time.Sleep(300 * time.Millisecond)
	}
	want := new(big.Int).Mul(big.NewInt(n), finney(100))
	waitFor(t, 120*time.Second, "all deposits credited", func() (bool, string) {
		mine(t, 1)
		g := u.bal(t, ethAsset).Total
		return g.Cmp(want) >= 0, g.String()
	})
	time.Sleep(8 * time.Second)
	if g := u.bal(t, ethAsset).Total; g.Cmp(want) != 0 {
		t.Errorf("DEPOSIT MISCOUNT: sent %s, ledger shows %s (double credit if higher, lost if lower)", want, g)
	}
}
