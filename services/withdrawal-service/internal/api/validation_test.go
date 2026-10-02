package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/withdrawal-service/internal/store"
)

type fakeValidator struct {
	reason string
	err    error
	calls  int
}

func (v *fakeValidator) Check(context.Context, store.Withdrawal) (string, error) {
	v.calls++
	return v.reason, v.err
}

func post(t *testing.T, f *fakeRequests, v Validator) *httptest.ResponseRecorder {
	t.Helper()
	req := withPeer(httptest.NewRequest("POST", "/v1/withdrawals", strings.NewReader(body)), "api-gateway")
	rec := httptest.NewRecorder()
	NewServer(f, nil, v, Config{}).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestValidRequestIsValidated(t *testing.T) {
	f, v := newFake(), &fakeValidator{}
	rec := post(t, f, v)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"status":"validated"`) || f.rows[[2]string{"alice", "k1"}].Status != store.StatusValidated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestFailedRuleRejectsWithReason(t *testing.T) {
	f, v := newFake(), &fakeValidator{reason: "below_minimum_withdrawal_amount"}
	rec := post(t, f, v)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 422 || got["status"] != "rejected" || got["reject_reason"] != "below_minimum_withdrawal_amount" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	row := f.rows[[2]string{"alice", "k1"}]
	if row.Status != store.StatusRejected || row.RejectReason == nil || *row.RejectReason != "below_minimum_withdrawal_amount" {
		t.Fatalf("row = %+v", row)
	}
	// A retry with the same key reads the verdict back, never re-validates (invariant #11).
	if rec := post(t, f, v); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reject_reason":"below_minimum_withdrawal_amount"`) || v.calls != 1 {
		t.Fatalf("retry: %d %s (validator calls %d)", rec.Code, rec.Body, v.calls)
	}
}

// No verdict: 503, the row stays `requested`, and a retry with the same key does not re-validate
// (option B: the key is spent, the Reaper fails the row later).
func TestNoVerdictLeavesTheRowRequested(t *testing.T) {
	f, v := newFake(), &fakeValidator{err: errors.New("wallet-service down")}
	rec := post(t, f, v)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "new idempotency_key") || f.rows[[2]string{"alice", "k1"}].Status != store.StatusRequested {
		t.Fatalf("%d %s, row %s", rec.Code, rec.Body, f.rows[[2]string{"alice", "k1"}].Status)
	}
	v.err = nil
	if rec := post(t, f, v); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"requested"`) || v.calls != 1 {
		t.Fatalf("retry with the spent key: %d %s (validator calls %d), want 200 requested and no re-validation", rec.Code, rec.Body, v.calls)
	}
}

// The Reaper won the race: the handler reports the row as it is, never overwrites it.
type reaperWins struct{ *fakeRequests }

func (r reaperWins) Transition(_ context.Context, id, _, _ string, _ *string) (store.Withdrawal, error) {
	for k, w := range r.rows {
		if w.ID == id {
			w.Status = store.StatusFailed
			r.rows[k] = w
		}
	}
	return store.Withdrawal{}, store.ErrStateChanged
}

func TestLostTransitionReportsTheCurrentStatus(t *testing.T) {
	f := newFake()
	req := withPeer(httptest.NewRequest("POST", "/v1/withdrawals", strings.NewReader(body)), "api-gateway")
	rec := httptest.NewRecorder()
	NewServer(reaperWins{f}, nil, &fakeValidator{}, Config{}).MTLSRoutes().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
