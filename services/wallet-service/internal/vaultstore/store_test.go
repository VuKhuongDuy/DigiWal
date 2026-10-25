package vaultstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const infraDir = "../../../../infra/postgres"

// asRole is the admin DSN with another login.
func asRole(t *testing.T, adminDSN, role, pw string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, pw
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// db builds the custody schema the way an environment has it: wallet-service's role OWNS the database and ran the migrations; the
// superuser then applies infra/postgres/sweep-signer-role.sql and vault.sql (the files themselves, read from the repo).
func db(t *testing.T) (admin *pgxpool.Pool, adminDSN string) {
	t.Helper()
	ctx := context.Background()
	adminDSN = testdb.New(t)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var dbname string
	if err := admin.QueryRow(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_wallet_t') THEN CREATE ROLE svc_wallet_t LOGIN PASSWORD 'w'; END IF; END $$`,
		`ALTER DATABASE ` + dbname + ` OWNER TO svc_wallet_t`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// wallet-service's own login runs the migrations (so it owns every table in public, like in dev)
	wcfg, _ := pgxpool.ParseConfig(adminDSN)
	wdsn := strings.Replace(adminDSN, wcfg.ConnConfig.User+":"+wcfg.ConnConfig.Password, "svc_wallet_t:w", 1)
	ws, err := storage.Connect(ctx, wdsn)
	if err != nil {
		t.Fatalf("migrations as the wallet-service role: %v", err)
	}
	t.Cleanup(ws.Close)

	// svc_sweepsigner as sweep-signer-role.sql makes it (its psql-only role-creation lines replaced by plain statements)
	sw, err := os.ReadFile(filepath.Join(infraDir, "sweep-signer-role.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, l := range strings.Split(string(sw), "\n") {
		if strings.Contains(l, `\gexec`) {
			continue
		}
		plain = append(plain, l)
	}
	for _, q := range []string{`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_sweepsigner') THEN CREATE ROLE svc_sweepsigner LOGIN PASSWORD 's'; END IF; END $$`} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, strings.ReplaceAll(strings.Join(plain, "\n"), "ON DATABASE custody", "ON DATABASE "+dbname)); err != nil {
		t.Fatalf("sweep-signer-role.sql: %v", err)
	}
	// vault.sql, with its one psql variable substituted
	v, err := os.ReadFile(filepath.Join(infraDir, "vault.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ReplaceAll(string(v), `:'pw_keyadmin'`, `'k'`)
	sql = strings.ReplaceAll(sql, "ON DATABASE custody", "ON DATABASE "+dbname)
	if _, err := admin.Exec(ctx, sql); err != nil {
		t.Fatalf("vault.sql: %v", err)
	}
	// idempotent: a second run changes nothing and does not fail
	if _, err := admin.Exec(ctx, sql); err != nil {
		t.Fatalf("vault.sql, second run: %v", err)
	}
	return admin, adminDSN
}

// denied runs q and wants it refused: insufficient_privilege (42501), or, for svc_sweepsigner, whose sessions are read-only on top of its
// grants, read_only_sql_transaction (25006) for a write.
func denied(t *testing.T, p *pgxpool.Pool, what, q string, args ...any) {
	t.Helper()
	_, err := p.Exec(context.Background(), q, args...)
	var pe *pgconn.PgError
	if err == nil || !errors.As(err, &pe) || (pe.Code != "42501" && pe.Code != "25006") {
		t.Errorf("%s: want a refusal (42501, or 25006 on a read-only session), got %v", what, err)
	}
}

func sample(version int, status string) Row {
	return Row{Role: "user_derivation", Version: version, Status: status, Purpose: "sweep", KEKVersion: 1, WrappedDEK: []byte("w"),
		Nonce: make([]byte, 12), Ciphertext: []byte("c"), AAD: []byte("a")}
}

// invariant 28: wallet-service's database role has NO access to the vault, although it owns the database.
func TestWalletServiceRoleCannotTouchTheVault(t *testing.T) {
	_, dsn := db(t)
	w := asRole(t, dsn, "svc_wallet_t", "w")
	ctx := context.Background()
	// it really is the owner-style role of the custody tables (so the denial below is the schema's, not a missing grant)
	if _, err := w.Exec(ctx, `SELECT count(*) FROM wallet_xpubs`); err != nil {
		t.Fatalf("the stand-in for wallet-service cannot read its own tables: %v", err)
	}
	for what, q := range map[string]string{
		"SELECT the table":    `SELECT * FROM vault.sealed_secrets`,
		"SELECT the view":     `SELECT * FROM vault.active_secrets`,
		"INSERT":              `INSERT INTO vault.sealed_secrets (role, version, status, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad) VALUES ('user_derivation', 1, 'active', 'sweep', 'kek-v1', 1, '\x00', '\x000000000000000000000000', '\x00', '\x00')`,
		"UPDATE":              `UPDATE vault.sealed_secrets SET status = 'retired'`,
		"DELETE":              `DELETE FROM vault.sealed_secrets`,
		"TRUNCATE":            `TRUNCATE vault.sealed_secrets`,
		"CREATE TABLE in it":  `CREATE TABLE vault.mine (a int)`,
		"DROP the schema":     `DROP SCHEMA vault CASCADE`,
		"ALTER the table":     `ALTER TABLE vault.sealed_secrets DISABLE TRIGGER ALL`,
		"GRANT itself access": `GRANT ALL ON vault.sealed_secrets TO svc_wallet_t`,
		"GRANT itself usage":  `GRANT USAGE ON SCHEMA vault TO svc_wallet_t`,
		"become svc_keyadmin": `SET ROLE svc_keyadmin`,
		"become sweepsigner":  `SET ROLE svc_sweepsigner`,
		"call the guard fn":   `SELECT vault.sealed_secrets_guard()`,
	} {
		denied(t, w, what, q)
	}
}

func TestKeyAdminRoleIsWriteOnlyThroughTheGuard(t *testing.T) {
	admin, dsn := db(t)
	ctx := context.Background()
	ka := asRole(t, dsn, "svc_keyadmin", "k")
	st := New(ka)
	if err := st.Insert(ctx, sample(1, Pending)); err != nil {
		t.Fatal(err)
	}
	if err := st.Insert(ctx, sample(1, Pending)); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate version: %v", err)
	}
	if err := st.Insert(ctx, sample(2, Pending)); err != nil {
		t.Fatal(err)
	}
	if err := st.Activate(ctx, "user_derivation", 1); err != nil {
		t.Fatal(err)
	}
	// the flip: version 2 active, version 1 retired, in one transaction; there is never a moment with two active rows
	if err := st.Activate(ctx, "user_derivation", 2); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.List(ctx, "user_derivation")
	if len(rows) != 2 || rows[0].Status != Retired || rows[1].Status != Active {
		t.Fatalf("rows %+v", rows)
	}
	// rollback: retired -> active
	if err := st.Activate(ctx, "user_derivation", 1); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.List(ctx, "user_derivation")
	if rows[0].Status != Active || rows[1].Status != Retired {
		t.Fatalf("after rollback %+v", rows)
	}
	if err := st.Activate(ctx, "user_derivation", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activating a missing version: %v", err)
	}
	// a pending row can be deleted, an active or retired one cannot (trigger), and no ciphertext column can ever change
	if err := st.Insert(ctx, sample(3, Pending)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePending(ctx, "user_derivation", 3); err != nil {
		t.Fatal(err)
	}
	denied(t, ka, "DELETE the active row", `DELETE FROM vault.sealed_secrets WHERE version = 1`)
	denied(t, ka, "DELETE a retired row", `DELETE FROM vault.sealed_secrets WHERE version = 2`)
	denied(t, ka, "UPDATE ciphertext", `UPDATE vault.sealed_secrets SET ciphertext = '\x01' WHERE version = 1`)
	denied(t, ka, "UPDATE wrapped_dek", `UPDATE vault.sealed_secrets SET wrapped_dek = '\x01' WHERE version = 1`)
	denied(t, ka, "UPDATE aad", `UPDATE vault.sealed_secrets SET aad = '\x01' WHERE version = 1`)
	denied(t, ka, "TRUNCATE", `TRUNCATE vault.sealed_secrets`)
	denied(t, ka, "ALTER TABLE", `ALTER TABLE vault.sealed_secrets DISABLE TRIGGER ALL`)
	denied(t, ka, "DROP TABLE", `DROP TABLE vault.sealed_secrets`)
	// a status can only move along the allowed edges: active -> pending is refused by the trigger even though the column is writable
	denied(t, ka, "active -> pending", `UPDATE vault.sealed_secrets SET status = 'pending' WHERE version = 1`)
	// at most one active row per role: the index says no even to a direct update
	_, err := ka.Exec(ctx, `UPDATE vault.sealed_secrets SET status = 'active' WHERE version = 2`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		t.Fatalf("a second active row: %v", err)
	}
	// svc_keyadmin cannot read custody data
	denied(t, ka, "SELECT wallet_xpubs", `SELECT * FROM wallet_xpubs`)
	denied(t, ka, "SELECT wallet_seeds", `SELECT * FROM wallet_seeds`)
	_ = admin
}

func TestSweepSignerRoleReadsOnlyTheActiveRowThroughTheView(t *testing.T) {
	_, dsn := db(t)
	ctx := context.Background()
	st := New(asRole(t, dsn, "svc_keyadmin", "k"))
	for _, v := range []int{1, 2} {
		if err := st.Insert(ctx, sample(v, Pending)); err != nil {
			t.Fatal(err)
		}
	}
	ss := asRole(t, dsn, "svc_sweepsigner", "s")
	var n int
	if err := ss.QueryRow(ctx, `SELECT count(*) FROM vault.active_secrets`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pending rows are visible to sweep-signer: %d %v", n, err)
	}
	if err := st.Activate(ctx, "user_derivation", 2); err != nil {
		t.Fatal(err)
	}
	var version int
	var wrapped, nonce, ct, aad []byte
	if err := ss.QueryRow(ctx, `SELECT version, wrapped_dek, gcm_nonce, ciphertext, aad FROM vault.active_secrets WHERE role = 'user_derivation'`).Scan(&version, &wrapped, &nonce, &ct, &aad); err != nil || version != 2 {
		t.Fatalf("active row: %v %v", version, err)
	}
	if err := st.Activate(ctx, "user_derivation", 1); err != nil {
		t.Fatal(err)
	}
	if err := ss.QueryRow(ctx, `SELECT version FROM vault.active_secrets`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("after the flip: %v %v", version, err)
	}
	for what, q := range map[string]string{
		"SELECT the table":   `SELECT * FROM vault.sealed_secrets`,
		"SELECT status":      `SELECT status FROM vault.sealed_secrets`,
		"INSERT":             `INSERT INTO vault.sealed_secrets (role, version, status, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad) VALUES ('user_derivation', 9, 'active', 'sweep', 'kek-v1', 1, '\x00', '\x000000000000000000000000', '\x00', '\x00')`,
		"UPDATE":             `UPDATE vault.sealed_secrets SET status = 'retired'`,
		"DELETE":             `DELETE FROM vault.sealed_secrets`,
		"write through view": `DELETE FROM vault.active_secrets`,
		"old seed column":    `SELECT seed_role, sealed_seed64 FROM wallet_seeds`,
	} {
		denied(t, ss, what, q)
	}
	_ = pgx.ErrNoRows
}
