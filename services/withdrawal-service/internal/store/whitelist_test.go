package store

import (
	"context"
	"testing"
	"time"
)

const addrA = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

func TestWhitelistAddIsIdempotentAndKeepsTheCooldownClock(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	e, created, err := s.AddWhitelistEntry(ctx, "alice", addrA)
	if err != nil || !created || e.UserID != "alice" || e.Address != addrA || !uuidRe.MatchString(e.ID) {
		t.Fatalf("add: %+v created=%v err=%v", e, created, err)
	}
	// Age the entry, then re-add: same entry, original added_at (invariant #15).
	if _, err := s.pool.Exec(ctx, `UPDATE whitelist_entries SET added_at = added_at - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	again, created, err := s.AddWhitelistEntry(ctx, "alice", addrA)
	if err != nil || created || again.ID != e.ID || !again.AddedAt.Equal(e.AddedAt.Add(-time.Hour)) {
		t.Fatalf("re-add: %+v created=%v err=%v (want id %s, added_at %s)", again, created, err, e.ID, e.AddedAt.Add(-time.Hour))
	}
	// Another user's entry for the same address is theirs alone.
	b, created, err := s.AddWhitelistEntry(ctx, "bob", addrA)
	if err != nil || !created || b.ID == e.ID {
		t.Fatalf("bob: %+v created=%v err=%v", b, created, err)
	}
}

// invariant #13: pending until the cooldown has passed since added_at, computed on read.
func TestWhitelistStatusFollowsTheCooldown(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const cooldown = 24 * time.Hour
	if _, ok, err := s.WhitelistStatus(ctx, "alice", addrA, cooldown); err != nil || ok {
		t.Fatalf("no entry: ok=%v err=%v", ok, err)
	}
	if _, _, err := s.AddWhitelistEntry(ctx, "alice", addrA); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		st, ok, err := s.WhitelistStatus(ctx, "alice", addrA, cooldown)
		if err != nil || !ok {
			t.Fatalf("status: ok=%v err=%v", ok, err)
		}
		return st
	}
	if got := status(); got != WhitelistPending {
		t.Fatalf("just added: %s", got)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE whitelist_entries SET added_at = now() - interval '23 hours 59 minutes'`); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != WhitelistPending {
		t.Fatalf("one minute short of the cooldown: %s", got)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE whitelist_entries SET added_at = now() - interval '24 hours'`); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != WhitelistActive {
		t.Fatalf("at exactly the cooldown: %s", got)
	}
	// Bob has no entry of his own, whatever alice has.
	if _, ok, _ := s.WhitelistStatus(ctx, "bob", addrA, cooldown); ok {
		t.Fatal("bob sees alice's entry")
	}
}

func TestWhitelistRemoveIsImmediateAndOwnOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	e, _, _ := s.AddWhitelistEntry(ctx, "alice", addrA)
	if _, err := s.pool.Exec(ctx, `UPDATE whitelist_entries SET added_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWhitelistEntry(ctx, "bob", e.ID); err != nil {
		t.Fatal(err)
	}
	if st, ok, _ := s.WhitelistStatus(ctx, "alice", addrA, 24*time.Hour); !ok || st != WhitelistActive {
		t.Fatal("bob removed alice's entry")
	}
	if err := s.RemoveWhitelistEntry(ctx, "alice", e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.WhitelistStatus(ctx, "alice", addrA, 24*time.Hour); ok {
		t.Fatal("an active entry survived removal")
	}
	if err := s.RemoveWhitelistEntry(ctx, "alice", e.ID); err != nil {
		t.Fatalf("second removal: %v, want a no-op", err)
	}
	// Re-adding after removal is a new entry with a fresh cooldown.
	again, created, _ := s.AddWhitelistEntry(ctx, "alice", addrA)
	if !created || again.ID == e.ID {
		t.Fatal("re-adding after removal did not start a new entry")
	}
	if st, _, _ := s.WhitelistStatus(ctx, "alice", addrA, 24*time.Hour); st != WhitelistPending {
		t.Fatalf("re-added entry is %s, want pending", st)
	}
}

func TestWhitelistSchemaRejectsMalformedAddresses(t *testing.T) {
	s := newStore(t)
	for _, a := range []string{"", "0x123", "70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x70997970C51812dc3A010C7d01b50e0d17dc79Cz"} {
		if _, _, err := s.AddWhitelistEntry(context.Background(), "alice", a); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
}
