package events

import (
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.TrimSpace(string(b)))
}

// The golden files pin the exact JSON wire shape (field names, types, order).
// If one of these fails, the wire contract changed: bump Version or fix the
// struct — don't just regenerate the file.
func testGolden[T any, P interface {
	*T
	Event
}](t *testing.T, name string) {
	t.Helper()
	want := golden(t, name)
	v, err := Decode[T, P](want)
	if err != nil {
		t.Fatalf("decoding golden %s: %v", name, err)
	}
	got, err := Encode(P(&v))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s wire shape changed\n got: %s\nwant: %s", name, got, want)
	}
	if P(&v).Topic() != name {
		t.Fatalf("topic %q, want %q", P(&v).Topic(), name)
	}
}

func TestGoldenWireShapes(t *testing.T) {
	testGolden[DepositDetected](t, TopicDepositsDetected)
	testGolden[DepositConfirmed](t, TopicDepositsConfirmed)
	testGolden[WithdrawalBroadcast](t, TopicWithdrawalsBroadcast)
	testGolden[WithdrawalConfirmed](t, TopicWithdrawalsConfirmed)
	testGolden[WithdrawalFailed](t, TopicWithdrawalsFailed)
}

func TestAmount(t *testing.T) {
	for _, ok := range []string{"0", "1", "1000000000000000000", "340282366920938463463374607431768211456"} { // > int64 and > uint64
		if err := Amount(ok).Validate(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-1", "+1", "1.5", "1e18", "0x10", " 1", "01", "one"} {
		if Amount(bad).Validate() == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	big2 := new(big.Int).Lsh(big.NewInt(1), 200)
	if got := AmountFromInt(big2).Int(); got.Cmp(big2) != 0 {
		t.Fatal("round trip through Amount lost precision")
	}
}

// amounts must survive as strings, never become JSON numbers
func TestAmountIsAStringOnTheWire(t *testing.T) {
	e := DepositConfirmed{}
	_ = json.Unmarshal(golden(t, TopicDepositsConfirmed), &e)
	out, _ := json.Marshal(e)
	if !strings.Contains(string(out), `"amount":"1000000000000000000"`) {
		t.Fatalf("amount not a string: %s", out)
	}
}

func TestDecodeRejectsBadPayloads(t *testing.T) {
	good := string(golden(t, TopicDepositsConfirmed))
	mutate := func(from, to string) []byte {
		if !strings.Contains(good, from) {
			t.Fatalf("test bug: %q not in payload", from)
		}
		return []byte(strings.Replace(good, from, to, 1))
	}
	cases := map[string][]byte{
		"unknown version":    mutate(`"version":1`, `"version":2`),
		"missing version":    mutate(`"version":1,`, ``),
		"bad tx hash":        mutate(`0x1111111111111111111111111111111111111111111111111111111111111111`, `0x1234`),
		"bad from address":   mutate(`"from_address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"`, `"from_address":"alice"`),
		"amount as number":   mutate(`"amount":"1000000000000000000"`, `"amount":1000000000000000000`),
		"negative amount":    mutate(`"amount":"1000000000000000000"`, `"amount":"-1"`),
		"log_index below -1": mutate(`"log_index":-1`, `"log_index":-2`),
		"asset not a uuid":   mutate(`11111111-1111-1111-1111-111111111111`, `native`),
		"missing user_id":    mutate(`"user_id":"alice",`, ``),
		"blank user_id":      mutate(`"user_id":"alice"`, `"user_id":"  "`),
		"not json":           []byte(`{`),
	}
	for name, payload := range cases {
		if _, err := Decode[DepositConfirmed](payload); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	// a producer adding an optional field must not break older consumers
	p := strings.Replace(string(golden(t, TopicDepositsConfirmed)), `"version":1`, `"version":1,"future_field":"x"`, 1)
	if _, err := Decode[DepositConfirmed]([]byte(p)); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeRefusesInvalidEvents(t *testing.T) {
	if _, err := Encode(DepositConfirmed{}); err == nil {
		t.Fatal("zero value must not encode")
	}
	e, _ := Decode[WithdrawalBroadcast](golden(t, TopicWithdrawalsBroadcast))
	e.BroadcastAt = time.Time{}
	if _, err := Encode(e); err == nil {
		t.Fatal("missing broadcast_at must not encode")
	}
}

func TestKeys(t *testing.T) {
	c, _ := Decode[DepositConfirmed](golden(t, TopicDepositsConfirmed))
	d, _ := Decode[DepositDetected](golden(t, TopicDepositsDetected))
	if c.Key() != "11155111:0x1111111111111111111111111111111111111111111111111111111111111111:-1" {
		t.Fatalf("deposit key %q", c.Key())
	}
	// same transfer -> same key on both topics, regardless of hex casing
	d.LogIndex = -1
	d.TxHash = "0x" + strings.Repeat("1", 64)
	if c.Key() != d.Key() {
		t.Fatalf("detected/confirmed keys differ: %q vs %q", d.Key(), c.Key())
	}
	upper := c
	upper.TxHash = strings.ToUpper(c.TxHash[:2]) + c.TxHash[2:]
	if upper.Key() != c.Key() {
		t.Fatal("key must be case-insensitive in tx_hash")
	}
	w, _ := Decode[WithdrawalBroadcast](golden(t, TopicWithdrawalsBroadcast))
	// invariant #20: keyed on the server-minted withdrawal_id, never the client's per-user key.
	if w.Key() != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("withdrawal key %q", w.Key())
	}
}

func TestBothDepositEventsRequireUserID(t *testing.T) {
	for _, name := range []string{TopicDepositsDetected, TopicDepositsConfirmed} {
		p := strings.Replace(string(golden(t, name)), `"user_id":"alice",`, ``, 1)
		var err error
		if name == TopicDepositsDetected {
			_, err = Decode[DepositDetected]([]byte(p))
		} else {
			_, err = Decode[DepositConfirmed]([]byte(p))
		}
		if err == nil {
			t.Errorf("%s decoded without a user_id: the ledger would have nobody to credit", name)
		}
	}
}

func TestWithdrawalFailed(t *testing.T) {
	good, err := Decode[WithdrawalFailed](golden(t, TopicWithdrawalsFailed))
	if err != nil {
		t.Fatal(err)
	}
	if good.Reason != "tx_reverted" || good.RevertReason == "" || good.Nonce == nil || good.Key() != good.WithdrawalID {
		t.Fatalf("%+v", good)
	}
	// Never signed: no tx_hash and no nonce, and the optional fields are absent on the wire.
	never := good
	never.Reason, never.RevertReason, never.TxHash, never.Nonce = "stale_request", "", "", nil
	b, err := Encode(never)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tx_hash", "nonce", "revert_reason"} {
		if strings.Contains(string(b), `"`+field+`"`) {
			t.Errorf("%s should be omitted: %s", field, b)
		}
	}
	for name, mutate := range map[string]func(*WithdrawalFailed){
		"no reason":               func(e *WithdrawalFailed) { e.Reason = "" },
		"bad tx hash":             func(e *WithdrawalFailed) { e.TxHash = "0x12" },
		"tx hash without a nonce": func(e *WithdrawalFailed) { e.Nonce = nil },
		"nonce without a tx hash": func(e *WithdrawalFailed) { e.TxHash = "" },
		"revert reason elsewhere": func(e *WithdrawalFailed) { e.Reason = "stale_request" },
		"no timestamp":            func(e *WithdrawalFailed) { e.FailedAt = time.Time{} },
		"bad withdrawal id":       func(e *WithdrawalFailed) { e.WithdrawalID = "x" },
	} {
		bad := good
		mutate(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
