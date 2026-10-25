package audit

import (
	"context"
	"strings"
	"testing"

	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/source"
)

const tHot2 = "0x00000000000000000000000000000000000000b1" // a second Hot address on the same chain

// newTwoAddrEnv is newTrEnv after wallet-service adopted the legacy rows for tHot (what a software-backend
// wallet-service does at start-up), plus a second Hot address tHot2 on the same chain with its own counter
// (next_nonce 10) and a signature at nonce 5, a nonce tHot never used.
func newTwoAddrEnv(t *testing.T) *trEnv {
	tr, _, _ := newTrEnv(t)
	tr.exec(tr.wal, `UPDATE hot_wallet_nonces SET hot_address = $1 WHERE hot_address = ''`, tHot)
	tr.exec(tr.wal, `UPDATE hot_wallet_signatures SET hot_address = $1 WHERE hot_address = ''`, tHot)
	raw, _ := signedTx(t, 5)
	tr.exec(tr.wal, `INSERT INTO hot_wallet_nonces (chain_id, hot_address, next_nonce) VALUES (31337, $1, 10)`, tHot2)
	tr.exec(tr.wal, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, hot_address, nonce, request_hash, signed_raw_tx, caller_cn)
		VALUES ('gas-topup:31337:0xbb:5', 31337, $1, 5, 'h', $2, 'sweeper')`, tHot2, raw)
	return tr
}

func (tr *trEnv) useHot(hot string) {
	tr.job.Tiers.Hot = hot
	tr.job.Src.Hot = hot
}

func TestPerAddressNextNonceIsIndependent(t *testing.T) {
	tr := newTwoAddrEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		hot  string
		want uint64
	}{{tHot, 3}, {tHot2, 10}, {strings.ToUpper(tHot2[:2]) + tHot2[2:], 10}} {
		src := *tr.job.Src
		src.Hot = c.hot
		n, ok, err := src.NextNonce(ctx)
		if err != nil || !ok || n != c.want {
			t.Errorf("NextNonce(%s) = %d %v %v, want %d", c.hot, n, ok, err, c.want)
		}
	}
	src := *tr.job.Src
	src.Hot = "0x00000000000000000000000000000000000000c1"
	if _, ok, err := src.NextNonce(ctx); err != nil || ok {
		t.Errorf("an address with no row has no nonce state: ok=%v err=%v", ok, err)
	}
	src.Hot = ""
	if _, _, err := src.NextNonce(ctx); err == nil {
		t.Error("an empty Hot address must be an error, never every row")
	}
}

func TestPerAddressKnownNoncesAreNotShared(t *testing.T) {
	tr := newTwoAddrEnv(t)
	a, err := tr.job.Src.Known(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !a.Nonces[0] || !a.Nonces[1] || a.Nonces[5] {
		t.Errorf("tHot knows nonces 0,1 only: %v", a.Nonces)
	}
	src := *tr.job.Src
	src.Hot = tHot2
	b, err := src.Known(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !b.Nonces[5] || b.Nonces[0] || b.Nonces[1] || len(b.Hashes) != 1 {
		t.Errorf("tHot2 knows nonce 5 only: %v %d hashes", b.Nonces, len(b.Hashes))
	}
}

// A gap (chain count above the allocated nonces) on one address must not be hidden by the other address's
// bigger counter, nor appear on the other address because of it.
func TestPerAddressGapOnOneDoesNotHideOrFakeTheOther(t *testing.T) {
	tr := newTwoAddrEnv(t)
	if res := tr.run(); res != job.OK {
		t.Fatalf("both consistent: %s %v", res, tr.fs())
	}

	// tHot sent 4 txs but only 3 nonces were allocated; tHot2's next_nonce (10) would hide it if mixed in
	tr.chain.count = 4
	if res := tr.run(); res != job.Fail || !tr.has("T1") {
		t.Fatalf("gap on tHot must fire: %s %v", res, tr.fs())
	}
	if _, ok := tr.fs()["T1 nonce:31337:"+tHot]; !ok {
		t.Fatalf("the finding must be referenced per address: %v", tr.fs())
	}

	// the same chain count is fine for tHot2 (next 10): no gap is invented there, and tHot's stays open
	tr.useHot(tHot2)
	if res := tr.run(); res != job.Fail {
		t.Fatalf("tHot's finding is not resolved by silence once tHot2 is audited: %s %v", res, tr.fs())
	}
	if _, ok := tr.fs()["T1 nonce:31337:"+tHot2]; ok {
		t.Fatalf("a gap faked on tHot2: %v", tr.fs())
	}

	// a real gap on tHot2 (11 txs vs next 10) fires on tHot2's own reference
	tr.chain.count = 11
	if res := tr.run(); res != job.Fail {
		t.Fatalf("%s", res)
	}
	if _, ok := tr.fs()["T1 nonce:31337:"+tHot2]; !ok {
		t.Fatalf("gap on tHot2 must fire: %v", tr.fs())
	}

	// back on tHot with a consistent count: tHot resolves; tHot2's finding (not re-checked now) stays open
	tr.useHot(tHot)
	tr.chain.count = 3
	tr.run()
	fs := tr.fs()
	if _, ok := fs["T1 nonce:31337:"+tHot]; ok {
		t.Errorf("tHot's gap resolved but still open: %v", fs)
	}
	if _, ok := fs["T1 nonce:31337:"+tHot2]; !ok {
		t.Errorf("tHot2's open gap must not be resolved by auditing tHot: %v", fs)
	}
}

// A tx from tHot at a nonce only tHot2 ever signed is not "a replacement of a signed nonce" (T2) but a tx
// wallet-service never signed for tHot (T4 when it moved value).
func TestPerAddressForeignNonceIsNotKnown(t *testing.T) {
	tr := newTwoAddrEnv(t)
	rogue := "0x" + strings.Repeat("ef", 32)
	tr.chain.sent[12] = []source.SentTx{{Hash: rogue, Nonce: 5, Block: 12}}
	tr.chain.mined[rogue] = 5
	if res := tr.run(); res != job.Fail || !tr.has("T2") {
		t.Fatalf("%s %v", res, tr.fs())
	}
	for k := range tr.fs() {
		if strings.HasPrefix(k, "T2 tx:") {
			return
		}
	}
	t.Fatalf("expected the 'no signature at all' T2: %v", tr.fs())
}

// Legacy (hot_address = '') rows: current Hot's while it has no row of its own; the previous key's after.
func TestLegacyRowsFollowTheCurrentHotOnlyUntilItHasItsOwn(t *testing.T) {
	tr, _, _ := newTrEnv(t) // legacy rows, next_nonce 3, two signatures
	ctx := context.Background()
	n, ok, err := tr.job.Src.NextNonce(ctx)
	if err != nil || !ok || n != 3 {
		t.Fatalf("legacy rows belong to the only Hot: %d %v %v", n, ok, err)
	}
	k, _ := tr.job.Src.Known(ctx)
	if len(k.Nonces) != 2 {
		t.Fatalf("legacy signatures known: %v", k.Nonces)
	}

	// a new key signs for the first time: its own row appears (seeded at 1) and the legacy rows stop counting
	tr.exec(tr.wal, `INSERT INTO hot_wallet_nonces (chain_id, hot_address, next_nonce) VALUES (31337, $1, 1)`, tHot2)
	src := *tr.job.Src
	src.Hot = tHot2
	if n, _, _ := src.NextNonce(ctx); n != 1 {
		t.Fatalf("the new key has its own counter: %d", n)
	}
	if k, _ := src.Known(ctx); len(k.Nonces) != 0 {
		t.Fatalf("legacy signatures are not the new key's: %v", k.Nonces)
	}
	// and the old key (tHot, which has no attributed row) still reads them as its own
	if n, _, _ := tr.job.Src.NextNonce(ctx); n != 3 {
		t.Fatalf("old key: %d", n)
	}
}
