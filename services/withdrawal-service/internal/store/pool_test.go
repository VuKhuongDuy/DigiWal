package store

import (
	"context"
	"testing"

	"digiwal/services/withdrawal-service/internal/testdb"
)

// The pool size is an explicit argument, so a DSN that the migrator also receives stays free of pool_* parameters.
func TestConnectWithPoolSetsMaxConns(t *testing.T) {
	dsn := testdb.New(t)
	st, err := ConnectWithPool(context.Background(), dsn, 12)
	if err != nil {
		t.Fatalf("ConnectWithPool: %v", err)
	}
	defer st.Close()
	if got := st.pool.Config().MaxConns; got != 12 {
		t.Errorf("MaxConns = %d, want 12", got)
	}
	st2, err := Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer st2.Close()
	if got := st2.pool.Config().MaxConns; got < 4 {
		t.Errorf("default MaxConns = %d, want pgx's default (>= 4)", got)
	}
}
