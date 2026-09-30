package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"digiwal/services/wallet-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

func TestMigration000002_CarriesExistingSeedIntoUserDerivation(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()

	m, err := NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(1); err != nil {
		t.Fatalf("migrate to baseline: %v", err)
	}

	// Seed the pre-migration schema the way a provisioned dev DB looks.
	sealed := []byte("sealed-seed-bytes")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO master_wallet (id, sealed_seed, next_index) VALUES (1, $1, 7)`, sealed); err != nil {
		t.Fatal(err)
	}

	if err := m.Migrate(2); err != nil {
		t.Fatalf("migrate to 000002: %v", err)
	}

	var got []byte
	var next int
	if err := conn.QueryRow(ctx, `SELECT sealed_seed, next_index FROM wallet_seeds WHERE seed_role = 'user_derivation'`).Scan(&got, &next); err != nil {
		t.Fatalf("user_derivation row missing: %v", err)
	}
	if !bytes.Equal(got, sealed) || next != 7 {
		t.Fatalf("got seed=%q next_index=%d, want %q / 7", got, next, sealed)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM wallet_seeds WHERE seed_role = 'hot_wallet'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("migration must not fabricate a hot_wallet row (n=%d err=%v)", n, err)
	}
	if _, err := conn.Exec(ctx, `SELECT 1 FROM master_wallet`); err == nil {
		t.Fatal("master_wallet should have been dropped")
	}

	// down restores the old shape and data.
	if err := m.Migrate(1); err != nil {
		t.Fatalf("migrate down to baseline: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT sealed_seed, next_index FROM master_wallet WHERE id = 1`).Scan(&got, &next); err != nil {
		t.Fatalf("master_wallet row missing after down: %v", err)
	}
	if !bytes.Equal(got, sealed) || next != 7 {
		t.Fatalf("after down: seed=%q next_index=%d", got, next)
	}
}

func TestMigration000002_NoopWhenNeverInitialized(t *testing.T) {
	dsn := testdb.New(t)
	if err := MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUp(dsn); err != nil { // idempotent
		t.Fatalf("second MigrateUp: %v", err)
	}
	s, err := Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SealedSeed(context.Background(), RoleUserDerivation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestInitSeed_OncePerRole(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InitSeed(ctx, RoleUserDerivation, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.InitSeed(ctx, RoleUserDerivation, []byte("b")); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second init: want ErrAlreadyInitialized, got %v", err)
	}
	if err := s.InitSeed(ctx, RoleHotWallet, []byte("h")); err != nil {
		t.Fatalf("hot_wallet is an independent role: %v", err)
	}
	a, _ := s.SealedSeed(ctx, RoleUserDerivation)
	h, _ := s.SealedSeed(ctx, RoleHotWallet)
	if string(a) != "a" || string(h) != "h" {
		t.Fatalf("roles mixed up: %q %q", a, h)
	}
	if err := s.InitSeed(ctx, "warm", []byte("x")); err == nil {
		t.Fatal("unknown role must be rejected by the CHECK constraint")
	}
}

func TestAllocateIndex_ConcurrentNeverDuplicates(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitSeed(ctx, RoleUserDerivation, []byte("a")); err != nil {
		t.Fatal(err)
	}
	// A hot_wallet row must not share the counter.
	if err := s.InitSeed(ctx, RoleHotWallet, []byte("h")); err != nil {
		t.Fatal(err)
	}

	const n = 100
	got := make([]uint32, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback(ctx)
			idx, err := s.allocateIndex(ctx, tx)
			if err != nil {
				t.Error(err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Error(err)
				return
			}
			got[i] = idx
		}()
	}
	wg.Wait()

	seen := map[uint32]bool{}
	for _, idx := range got {
		if seen[idx] {
			t.Fatalf("index %d allocated twice", idx)
		}
		seen[idx] = true
	}
	for i := uint32(0); i < n; i++ {
		if !seen[i] {
			t.Fatalf("index %d never allocated (indices must be dense 0..%d)", i, n-1)
		}
	}
}

func TestUserForAddress_IsTheReverseOfCreateAddress(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitSeed(ctx, RoleUserDerivation, []byte("a")); err != nil {
		t.Fatal(err)
	}
	addr, err := s.CreateAddress(ctx, "alice", "evm", func(i uint32) (string, error) { return fmt.Sprintf("0x%040x", i+1), nil })
	if err != nil {
		t.Fatal(err)
	}
	user, chain, err := s.UserForAddress(ctx, addr)
	if err != nil || user != "alice" || chain != "evm" {
		t.Fatalf("got %q %q %v", user, chain, err)
	}
	if _, _, err := s.UserForAddress(ctx, "0x"+strings.Repeat("9", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown address: %v", err)
	}
}
