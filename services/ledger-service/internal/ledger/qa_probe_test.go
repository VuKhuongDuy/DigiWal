package ledger

import (
	"context"
	"errors"
	"math/big"
	"testing"
)

// QA probes (tester role). Each test states the behaviour the design implies; a failure is a finding.

// ledger_transactions.idempotency_key is one namespace shared by deposits, corrections and Settle
// (Settle reuses the Lock's key). A Settle whose key is already taken must not wedge the Lock.
func TestQA_SettleKeyAlreadyUsedByAnotherTransaction_DoesNotWedgeTheLock(t *testing.T) {
	t.Skip("open finding L-1 — remove this line once fixed")
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustApply(t, s, deposit("w1", "bob", "5")) // someone else's ledger tx already owns key "w1"
	mustLock(t, s, "w1", "alice", "400")
	_, err := s.Settle(ctx, "w1")
	t.Logf("settle with colliding key -> err=%v", err)
	if err == nil {
		t.Fatal("expected some error")
	}
	// The lock must still be resolvable: at minimum Release has to work, or funds are locked forever.
	if err := s.Release(ctx, "w1"); err != nil {
		t.Fatalf("lock is stuck: release failed too: %v", err)
	}
}

// The reverse: a withdrawal's Settle taking key K first makes a later legitimate deposit with key K unpostable.
func TestQA_SettledWithdrawalKeyBlocksALaterDepositWithTheSameKey(t *testing.T) {
	t.Skip("open finding L-1 — remove this line once fixed")
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "1:0xabc:-1", "alice", "10")
	mustSettle(t, s, "1:0xabc:-1")
	tx := deposit("x", "bob", "50")
	tx.IdempotencyKey = "1:0xabc:-1"
	if _, err := s.Apply(context.Background(), tx); err != nil {
		t.Fatalf("legit deposit rejected because a withdrawal used the same key string: %v", err)
	}
}

// A "deposit" is by definition chain -> user. Apply doesn't enforce the shape, so a deposit can move funds
// user -> user or user -> chain with no Lock.
func TestQA_DepositShapeIsNotEnforced(t *testing.T) {
	t.Skip("open finding L-2 — remove this line once fixed")
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	tx := Transaction{IdempotencyKey: "evil", Source: SourceDeposit,
		Deposit: &DepositRef{ChainID: 1, TxHash: "0xaa", LogIndex: 0},
		Entries: []Entry{user("alice", eth, "-1000"), user("bob", eth, "1000")}}
	_, err := s.Apply(ctx, tx)
	if err == nil {
		t.Fatal("a 'deposit' moved alice's funds to bob (user->user), no chain leg, no lock")
	}
}

func TestQA_NULByteInIdentifiersIsA400NotA500(t *testing.T) {
	t.Skip("open finding L-4 — remove this line once fixed")
	s, ctx := newStore(t), context.Background()
	_, err := s.Lock(ctx, lockReq("k\x00", "alice", "1"))
	if err != nil && !errors.Is(err, ErrInvalid) {
		t.Fatalf("NUL in idempotency_key surfaces as internal error, not ErrInvalid: %v", err)
	}
	_, err = s.Lock(ctx, lockReq("k2", "al\x00ice", "1"))
	if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("NUL in user_id surfaces as internal error: %v", err)
	}
}

// Two uint256-max deposits overflow NUMERIC(78,0). Consumer treats a non-domain error as transient => retries forever.
func TestQA_BalanceOverflowIsADomainErrorNotATransientOne(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	mustApply(t, s, deposit("m1", "alice", max))
	_, err := s.Apply(ctx, deposit("m2", "alice", max))
	t.Logf("second max deposit -> %v", err)
	if err != nil && !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow is an untyped error (consumer would retry it forever): %v", err)
	}
}

// Deactivating an asset must not strand in-flight Locks.
func TestQA_DeactivatedAssetStillLetsInFlightLocksResolve(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "100")
	mustLock(t, s, "w2", "alice", "100")
	if err := s.SyncAssets(ctx, registry()[1:]); err != nil { // ETH leaves the registry
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, "w1"); err != nil {
		t.Fatalf("settle after deactivation: %v", err)
	}
	if err := s.Release(ctx, "w2"); err != nil {
		t.Fatalf("release after deactivation: %v", err)
	}
}

// SyncAssets silently rewrites decimals / type / contract of an asset that already has balances.
func TestQA_SyncAssetsRefusesToRedefineAnAssetThatHasBalances(t *testing.T) {
	t.Skip("open finding L-3 — remove this line once fixed")
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	r := registry()
	r[0].Decimals = 6
	r[0].Symbol = "USDC"
	if err := s.SyncAssets(ctx, r); err == nil {
		t.Fatal("registry changed decimals 18->6 and symbol under existing balances without complaint")
	}
}

// Correction reversing a Settle leg, then the reversal itself being corrected (re-debit) must respect Available.
func TestQA_CorrectionOfACorrectionRoundTrips(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	mustSettle(t, s, "w1")
	mustApply(t, s, correction("c1", entryIDs(t, s, "w1")...))
	bal(t, s, "alice", "1000", "0", "1000")
	mustApply(t, s, correction("c2", entryIDs(t, s, "c1")...))
	bal(t, s, "alice", "600", "0", "600")
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-600" {
		t.Fatalf("chain %s", c)
	}
}
