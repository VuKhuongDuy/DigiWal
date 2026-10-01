package api

import (
	"context"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
)

type fakeReserver struct {
	err   error
	keys  []string
	calls int
}

func (r *fakeReserver) Lock(_ context.Context, key, userID, assetID string, amount *big.Int) error {
	r.calls++
	r.keys = append(r.keys, key)
	return r.err
}

func postWithLedger(t *testing.T, f *fakeRequests, v Validator, res Reserver) *httptest.ResponseRecorder {
	t.Helper()
	req := withPeer(httptest.NewRequest("POST", "/v1/withdrawals", strings.NewReader(body)), "api-gateway")
	rec := httptest.NewRecorder()
	NewServer(f, nil, v, Config{Reserver: res}).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestReservedAfterTheLock(t *testing.T) {
	f, res := newFake(), &fakeReserver{}
	rec := postWithLedger(t, f, passAll{}, res)
	row := f.rows[[2]string{"alice", "k1"}]
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"status":"reserved"`) || row.Status != store.StatusReserved {
		t.Fatalf("%d %s (row %s)", rec.Code, rec.Body, row.Status)
	}
	// The ledger only ever sees the Withdrawal Key, never the client's key (invariant #20).
	if len(res.keys) != 1 || res.keys[0] != "withdrawal:"+row.ID {
		t.Fatalf("lock keys = %v", res.keys)
	}
}

func TestInsufficientFundsIsARejection(t *testing.T) {
	f, res := newFake(), &fakeReserver{err: ledger.ErrInsufficientFunds}
	rec := postWithLedger(t, f, passAll{}, res)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"reject_reason":"insufficient_funds"`) || f.rows[[2]string{"alice", "k1"}].Status != store.StatusRejected {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestLedgerDownLeavesTheRowValidated(t *testing.T) {
	f, res := newFake(), &fakeReserver{err: errors.New("connection refused")}
	rec := postWithLedger(t, f, passAll{}, res)
	if rec.Code != 503 || f.rows[[2]string{"alice", "k1"}].Status != store.StatusValidated {
		t.Fatalf("%d %s (row %s), want 503 and the row left validated for the Reaper", rec.Code, rec.Body, f.rows[[2]string{"alice", "k1"}].Status)
	}
	// A retry with the spent key reads the row back and does not lock again.
	res.err = nil
	if rec := postWithLedger(t, f, passAll{}, res); rec.Code != 200 || res.calls != 1 {
		t.Fatalf("retry: %d (lock calls %d)", rec.Code, res.calls)
	}
}

func TestRejectedRequestsNeverReachTheLedger(t *testing.T) {
	f, res := newFake(), &fakeReserver{}
	if rec := postWithLedger(t, f, &fakeValidator{reason: "destination_not_whitelisted"}, res); rec.Code != 422 || res.calls != 0 {
		t.Fatalf("%d (lock calls %d)", rec.Code, res.calls)
	}
	if rec := postWithLedger(t, newFake(), &fakeValidator{err: errors.New("down")}, res); rec.Code != 503 || res.calls != 0 {
		t.Fatalf("no verdict: %d (lock calls %d)", rec.Code, res.calls)
	}
}
