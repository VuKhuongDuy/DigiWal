package api

import (
	"net/url"
	"testing"
)

// A Lock key is any string <=256 chars, but Settle/Release address it as a URL path segment.
func TestQA_KeysWithSlashOrSpecialCharsCanStillBeSettled(t *testing.T) {
	t.Skip("open finding L-5 — remove this line once fixed")
	for _, key := range []string{"a/b", "1:0xabc:-1", "a b", "a?b", "a%b", "über", "..", "."} {
		f := &fake{}
		rec := call(t, f, "withdrawal-service", "/v1/ledger/locks/"+url.PathEscape(key)+"/settle", ``)
		t.Logf("key %q -> HTTP %d, ledger saw %q", key, rec.Code, f.gotKey)
		if f.gotKey != key {
			t.Errorf("key %q unreachable/mangled via settle (got %q, status %d)", key, f.gotKey, rec.Code)
		}
	}
}

func TestQA_AmountFormats(t *testing.T) {
	for _, amt := range []string{"+5", "5.0", "0x10", "1e18", " 5", "5 ", "", "-5", "0", "١٢٣", "1_000"} {
		f := &fake{}
		body := `{"idempotency_key":"k","user_id":"u","asset_id":"` + eth + `","amount":"` + amt + `"}`
		call(t, f, "withdrawal-service", "/v1/ledger/locks", body)
		if f.calls > 0 {
			t.Logf("amount %q REACHED the ledger as %s", amt, f.gotLock.Amount)
		}
	}
}

func TestQA_TrailingGarbageAfterJSONIsRejected(t *testing.T) {
	t.Skip("open finding L-5 — remove this line once fixed")
	f := &fake{}
	call(t, f, "withdrawal-service", "/v1/ledger/locks", lockBody+`{"x":1}`)
	if f.calls > 0 {
		t.Fatal("trailing JSON value accepted")
	}
}
