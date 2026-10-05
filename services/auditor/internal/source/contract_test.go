package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"digiwal/services/auditor/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

// applyDir runs a service's *.up.sql migrations, in order, on db.
func applyDir(t *testing.T, ctx context.Context, db *pgxpool.Pool, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations in %s (%v)", dir, err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func applyFile(t *testing.T, ctx context.Context, db *pgxpool.Pool, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(b)); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// For each foreign store: the service's own migrations are applied to a fresh database, and then
//  1. every table and column the auditor reads (Contract) exists  -> a migration that renames or drops
//     one breaks this test, which each service's `make auditor-contract` runs;
//  2. as auditor_ro, each Contract column is selectable, and NOTHING else is (tables, columns), so the
//     roles file and Contract cannot drift apart and key material stays unreadable;
//  3. as auditor_ro, no write of any kind succeeds.
func TestStoreContractAndRoles(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	ap, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	applyFile(t, ctx, ap, "../../roles/00_role.sql")

	for _, st := range Stores {
		t.Run(st.Name, func(t *testing.T) {
			dsn := testdb.New(t)
			db, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			applyDir(t, ctx, db, filepath.Join("..", "..", st.MigrationsDir))
			applyFile(t, ctx, db, filepath.Join("..", "..", st.RolesFile))

			want := Contract[st.Name]

			// 1. the schema still has what the auditor reads
			for table, cols := range want {
				for _, c := range cols {
					var n int
					err := db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2`, table, c).Scan(&n)
					if err != nil || n != 1 {
						t.Errorf("schema contract broken: %s.%s.%s is missing (a service migration changed what the auditor reads)", st.Name, table, c)
					}
				}
			}

			// 2. exact privileges, as auditor_ro
			ro, err := pgxpool.New(ctx, testdb.WithUser(t, dsn, dbName(t, dsn), "auditor_ro", "auditor_ro"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			rows, err := db.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
				WHERE table_schema='public' AND table_name <> 'schema_migrations' ORDER BY 1, ordinal_position`)
			if err != nil {
				t.Fatal(err)
			}
			type tc struct{ table, col string }
			var all []tc
			for rows.Next() {
				var x tc
				if err := rows.Scan(&x.table, &x.col); err != nil {
					t.Fatal(err)
				}
				all = append(all, x)
			}
			rows.Close()
			allowed := func(x tc) bool {
				for _, c := range want[x.table] {
					if c == x.col {
						return true
					}
				}
				return false
			}
			for _, x := range all {
				_, err := ro.Exec(ctx, fmt.Sprintf(`SELECT %q FROM %q LIMIT 0`, x.col, x.table))
				switch {
				case allowed(x) && err != nil:
					t.Errorf("auditor_ro cannot read %s.%s.%s but the contract says it reads it: %v", st.Name, x.table, x.col, err)
				case !allowed(x) && err == nil:
					t.Errorf("auditor_ro can read %s.%s.%s which is not in the contract (least privilege)", st.Name, x.table, x.col)
				}
			}
			for _, k := range ForbiddenTables {
				if _, err := ro.Exec(ctx, "SELECT 1 FROM "+k+" LIMIT 0"); err == nil {
					t.Errorf("auditor_ro can read %s", k)
				}
			}

			// 3. nothing can be written, even into a table it can read
			for table, cols := range want {
				for _, stmt := range []string{
					fmt.Sprintf("DELETE FROM %s", table),
					fmt.Sprintf("UPDATE %s SET %s = %s", table, cols[0], cols[0]),
					fmt.Sprintf("TRUNCATE %s", table),
				} {
					if _, err := ro.Exec(ctx, stmt); err == nil {
						t.Errorf("auditor_ro executed %q on %s", stmt, st.Name)
					}
				}
			}
			if _, err := ro.Exec(ctx, "CREATE TABLE auditor_scratch (a int)"); err == nil {
				t.Errorf("auditor_ro created a table in %s", st.Name)
			}
		})
	}
}

// The PG wrapper opens sessions read-only and refuses non-SELECT before sending.
func TestOpenPGIsReadOnly(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.New(t)
	pg, err := OpenPG(ctx, "test", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	var ro string
	if err := pg.QueryRow(ctx, "SELECT current_setting('default_transaction_read_only')").Scan(&ro); err != nil || ro != "on" {
		t.Fatalf("session not read-only: %q %v", ro, err)
	}
	if _, err := pg.Query(ctx, "CREATE TABLE x (a int)"); err == nil {
		t.Error("a non-SELECT must be refused by the wrapper")
	}
	if err := pg.QueryRow(ctx, "DELETE FROM x").Scan(); err == nil {
		t.Error("QueryRow must refuse non-SELECT")
	}
	err = pg.Snapshot(ctx, func(q Querier) error {
		var one int
		if err := q.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		if _, err := q.Query(ctx, "UPDATE y SET a = 1"); err == nil {
			return fmt.Errorf("snapshot accepted an UPDATE")
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
	// even bypassing the wrapper, the session itself rejects writes
	raw, _ := pgx.Connect(ctx, dsn+"&options=-c%20default_transaction_read_only%3Don")
	if raw != nil {
		defer raw.Close(ctx)
		if _, err := raw.Exec(ctx, "CREATE TABLE z (a int)"); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("read-only session accepted a write: %v", err)
		}
	}
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ConnConfig.Database
}
