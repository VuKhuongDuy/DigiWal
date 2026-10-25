package ledger

import (
	"context"
	"testing"

	"digiwal/services/ledger-service/internal/testdb"
)

// The pool size is an explicit argument, so a DSN that the migrator also receives stays free of pool_* parameters.
func TestConnectWithPoolSetsMaxConns(t *testing.T) {
	dsn := testdb.New(t)
	s, err := ConnectWithPool(context.Background(), dsn, 12)
	if err != nil {
		t.Fatalf("ConnectWithPool: %v", err)
	}
	defer s.Close()
	if got := s.pool.Config().MaxConns; got != 12 {
		t.Errorf("MaxConns = %d, want 12", got)
	}
}
