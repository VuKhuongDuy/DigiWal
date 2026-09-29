package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/ledger-service/internal/ledger"
)

type fake struct {
	status ledger.Status
	err    error
	got    ledger.Transaction
	calls  int
}

func (f *fake) Apply(_ context.Context, t ledger.Transaction) (ledger.Status, error) {
	f.calls++
	f.got = t
	return f.status, f.err
}

func post(t *testing.T, f *fake, cn, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/ledger/transactions", strings.NewReader(body))
	if cn != "" {
		cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	rec := httptest.NewRecorder()
	NewServer(f).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

const eth = "11111111-1111-1111-1111-111111111111"

var body = `{"idempotency_key":"k","source_type":"withdrawal","entries":[
	{"account_ref":"user","user_id":"alice","asset_id":"` + eth + `","amount":"-500"},
	{"account_ref":"chain","asset_id":"` + eth + `","amount":"500"}]}`

// service-auth-mtls.md invariant #7: only withdrawal-service may post transactions.
func TestApply_OnlyWithdrawalServiceMayCall(t *testing.T) {
	for cn, want := range map[string]int{"withdrawal-service": 200, "sweeper": 403, "api-gateway": 403, "chain-watcher": 403, "": 403} {
		f := &fake{status: ledger.Applied}
		if got := post(t, f, cn, body).Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
		if want == 403 && f.calls != 0 {
			t.Errorf("CN=%q reached the ledger", cn)
		}
	}
}

func TestApply_ParsesWireRequest(t *testing.T) {
	f := &fake{status: ledger.Applied}
	rec := post(t, f, "withdrawal-service", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"applied"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f.got.IdempotencyKey != "k" || f.got.Source != ledger.SourceWithdrawal || len(f.got.Entries) != 2 ||
		f.got.Entries[0].Amount.String() != "-500" || f.got.Entries[0].Ref != ledger.RefUser || f.got.Entries[1].Ref != ledger.RefChain {
		t.Fatalf("parsed wrong: %+v", f.got)
	}
	f = &fake{status: ledger.AlreadyApplied}
	if rec := post(t, f, "withdrawal-service", body); !strings.Contains(rec.Body.String(), `"status":"already_applied"`) {
		t.Fatalf("%s", rec.Body)
	}
}

func TestApply_ErrorMapping(t *testing.T) {
	cases := map[error]int{
		ledger.ErrInsufficientFunds:    422,
		ledger.ErrIdempotencyKeyReused: 409,
		ledger.ErrInvalid:              400,
		ledger.ErrUnbalanced:           400,
		ledger.ErrUnknownAsset:         400,
		errors.New("db down"):          500,
	}
	for err, want := range cases {
		rec := post(t, &fake{err: err}, "withdrawal-service", body)
		if rec.Code != want {
			t.Errorf("%v → %d, want %d", err, rec.Code, want)
		}
	}
	if rec := post(t, &fake{err: ledger.ErrInsufficientFunds}, "withdrawal-service", body); !strings.Contains(rec.Body.String(), `"status":"insufficient_funds"`) {
		t.Fatalf("422 body: %s", rec.Body)
	}
}

func TestApply_RejectsBadRequestsBeforeTheLedger(t *testing.T) {
	bad := map[string]string{
		"not json":            `nope`,
		"unknown field":       `{"idempotency_key":"k","source_type":"withdrawal","entries":[],"bonus":1}`,
		"deposit source":      strings.Replace(body, `"withdrawal"`, `"deposit"`, 1), // must go through the screened consumer
		"unknown source":      strings.Replace(body, `"withdrawal"`, `"gift"`, 1),
		"amount as number":    strings.Replace(body, `"-500"`, `-500`, 1),
		"amount not a number": strings.Replace(body, `"-500"`, `"abc"`, 1),
	}
	for name, b := range bad {
		f := &fake{}
		if rec := post(t, f, "withdrawal-service", b); rec.Code != 400 || f.calls != 0 {
			t.Errorf("%s: %d (ledger calls %d), want 400 and 0", name, rec.Code, f.calls)
		}
	}
}

func TestHealthzIsOnThePlainListener(t *testing.T) {
	rec := httptest.NewRecorder()
	NewServer(&fake{}).Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	NewServer(&fake{}).Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/ledger/transactions", strings.NewReader(body)))
	if rec.Code == 200 {
		t.Fatal("the apply endpoint must not exist on the plain listener")
	}
}
