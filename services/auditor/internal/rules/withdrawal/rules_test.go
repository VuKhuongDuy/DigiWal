package withdrawal

import (
	"encoding/hex"
	"math/big"
	"sort"
	"strings"
	"testing"

	"digiwal/services/auditor/internal/finding"
)

const (
	id       = "11111111-1111-1111-1111-111111111111"
	asset    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dest     = "0x00000000000000000000000000000000000000d1"
	token    = "0x00000000000000000000000000000000000000c0"
	txHash   = "0xabc0000000000000000000000000000000000000000000000000000000000001"
	otherTx  = "0xabc0000000000000000000000000000000000000000000000000000000000002"
	settleID = "22222222-2222-2222-2222-222222222222"
)

func i64(n int64) *int64  { return &n }
func bi(n int64) *big.Int { return big.NewInt(n) }

// transferInput is the calldata of transfer(to, amount).
func transferInput(to string, amount int64) []byte {
	b, _ := hex.DecodeString(transferSelector)
	b = append(b, make([]byte, 12)...)
	t, _ := hex.DecodeString(strings.TrimPrefix(to, "0x"))
	b = append(b, t...)
	a := make([]byte, 32)
	bi(amount).FillBytes(a)
	return append(b, a...)
}

// confirmedERC20 is a fully consistent confirmed ERC-20 withdrawal of 1000 units.
func confirmedERC20() Facts {
	return Facts{
		Req: &Request{ID: id, UserID: "u1", AssetID: asset, Amount: bi(1000), ToAddress: dest, Status: StatusConfirmed,
			TxHash: txHash, Nonce: i64(7), ChainID: i64(31337), ConfirmedBlock: i64(50), SettleLedgerTxID: settleID, GasPrice: bi(1_000_000_000)},
		Lock:          &Lock{Status: "settled", Amount: bi(1000), AssetID: asset, UserID: "u1"},
		Settle:        &Settle{ID: settleID, Entries: []Entry{{Amount: bi(-1000), Kind: "user", AssetID: asset}, {Amount: bi(1000), Kind: "chain", AssetID: asset}}},
		GasFeePurpose: PurposeWithdrawal,
		Sig:           &Signature{Nonce: 7, CallerCN: SignerCN},
		History:       &History{Status: "confirmed", TxHash: txHash, Amount: "1000", AssetID: asset},
		Asset:         &Asset{Type: "erc20", ContractAddress: token},
		Chain:         map[string]ChainTx{txHash: {Found: true, Mined: true, Success: true, Block: 50, To: token, Value: bi(0), Input: transferInput(dest, 1000)}},
	}
}

func confirmedNative() Facts {
	f := confirmedERC20()
	f.Asset = &Asset{Type: "native"}
	f.Chain = map[string]ChainTx{txHash: {Found: true, Mined: true, Success: true, Block: 50, To: dest, Value: bi(1000)}}
	return f
}

// failedReverted is a request whose tx was mined and reverted.
func failedReverted() Facts {
	f := confirmedERC20()
	f.Req.Status, f.Req.RejectReason, f.Req.SettleLedgerTxID, f.Req.ConfirmedBlock = StatusFailed, ReasonTxReverted, "", nil
	f.Lock.Status = "released"
	f.Settle = nil
	f.GasFeePurpose = PurposeWithdrawalReverted
	f.History = &History{Status: "failed", TxHash: txHash, Amount: "1000", AssetID: asset}
	f.Chain = map[string]ChainTx{txHash: {Found: true, Mined: true, Success: false, Block: 50}}
	return f
}

func failedBeforeSigning() Facts {
	return Facts{
		Req:     &Request{ID: id, AssetID: asset, Amount: bi(1000), ToAddress: dest, Status: StatusFailed, RejectReason: "stale_reservation"},
		Lock:    &Lock{Status: "released", Amount: bi(1000), AssetID: asset},
		History: &History{Status: "failed", Amount: "1000", AssetID: asset},
	}
}

func refunded() Facts {
	f := confirmedERC20()
	f.Req.Status, f.Req.RejectReason = StatusFailed, "abandoned_refunded"
	f.Refunds = 1
	f.History.Status = "failed"
	return f
}

func rejected() Facts {
	return Facts{Req: &Request{ID: id, AssetID: asset, Amount: bi(1000), ToAddress: dest, Status: StatusRejected, RejectReason: "not_whitelisted"}}
}

func rulesOf(vs []finding.Violation) []string {
	var r []string
	for _, v := range vs {
		r = append(r, v.Rule)
	}
	sort.Strings(r)
	return r
}

func expect(t *testing.T, name string, f Facts, want ...string) []finding.Violation {
	t.Helper()
	vs := Evaluate(f)
	got := rulesOf(vs)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: violated %v, want %v (%+v)", name, got, want, vs)
	}
	return vs
}

func TestConsistentWithdrawalsViolateNothing(t *testing.T) {
	for name, f := range map[string]Facts{
		"confirmed erc20": confirmedERC20(), "confirmed native": confirmedNative(), "failed reverted": failedReverted(),
		"failed before signing": failedBeforeSigning(), "failed after refund": refunded(), "rejected": rejected(),
	} {
		expect(t, name, f)
	}
}

func TestEveryRuleIsListed(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range []Facts{confirmedERC20(), failedReverted()} {
		mut := []func(*Facts){
			func(f *Facts) { f.Settle = nil }, func(f *Facts) { f.Lock.Amount = bi(1) }, func(f *Facts) { f.History = nil },
			func(f *Facts) { f.Sig = nil }, func(f *Facts) { f.Chain = nil }, func(f *Facts) { f.SharedBy = []string{"x"} },
			func(f *Facts) { f.GasFeePurpose = "" }, func(f *Facts) { f.Req.Status = StatusRejected },
		}
		for _, m := range mut {
			g := f
			rc := *f.Req
			g.Req = &rc
			lk := *f.Lock
			g.Lock = &lk
			m(&g)
			for _, v := range Evaluate(g) {
				seen[v.Rule] = true
			}
		}
	}
	for r := range seen {
		found := false
		for _, l := range Rules {
			found = found || l == r
		}
		if !found {
			t.Errorf("rule %s is produced but not in Rules", r)
		}
	}
}

func TestW1(t *testing.T) {
	f := confirmedERC20()
	f.Settle = nil
	expect(t, "confirmed without a Settle", f, "W1")

	f = confirmedERC20()
	f.Req.SettleLedgerTxID = "33333333-3333-3333-3333-333333333333"
	expect(t, "settle_ledger_tx_id points elsewhere", f, "W1")
}

func TestW2(t *testing.T) {
	f := rejected()
	f.Settle = &Settle{ID: settleID}
	expect(t, "Settle for a rejected request (also a Settle never refunded)", f, "W2", "W4")

	f = confirmedERC20()
	f.Refunds = 1
	expect(t, "refunded but still confirmed", f, "W2")

	expect(t, "orphan Settle (no request)", Facts{Settle: &Settle{ID: settleID}}, "W2")
	expect(t, "nothing at all", Facts{})
}

func TestW3(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"lock amount":    func(f *Facts) { f.Lock.Amount = bi(999) },
		"settle amount":  func(f *Facts) { f.Settle.Entries[0].Amount = bi(-1001) },
		"history amount": func(f *Facts) { f.History.Amount = "1001" },
		"lock asset":     func(f *Facts) { f.Lock.AssetID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" },
		"history asset":  func(f *Facts) { f.History.AssetID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" },
	} {
		f := confirmedERC20()
		mut(&f)
		expect(t, name, f, "W3")
	}
	f := confirmedERC20()
	f.History.AssetID = strings.ToUpper(asset)
	expect(t, "asset compared case-insensitively", f)

	f = confirmedERC20()
	f.Lock = nil
	expect(t, "confirmed with no Lock", f, "W3")
}

func TestW4(t *testing.T) {
	f := failedBeforeSigning()
	f.Lock.Status = "active"
	expect(t, "failed but Lock active", f, "W4")

	f = rejected()
	f.Lock = &Lock{Status: "released", Amount: bi(1000), AssetID: asset}
	expect(t, "rejected request with a Lock", f, "W4")

	f = refunded()
	f.Refunds = 0
	expect(t, "Settled, failed, not refunded", f, "W4")
	f.Refunds = 2
	expect(t, "refunded twice", f, "W4")
}

func TestW5(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"no document":    func(f *Facts) { f.History = nil },
		"status differs": func(f *Facts) { f.History.Status = "broadcast" },
		"hash differs":   func(f *Facts) { f.History.TxHash = otherTx },
	} {
		f := confirmedERC20()
		mut(&f)
		vs := expect(t, name, f, "W5")
		if len(vs) == 1 && (vs[0].Severity != finding.Warn || vs[0].EscalateTo != finding.Critical || vs[0].Immediate) {
			t.Errorf("%s: W5 must be a warn that escalates and waits for a second pass: %+v", name, vs[0])
		}
	}
	f := confirmedERC20()
	f.History.TxHash = strings.ToUpper(txHash[2:])
	f.History.TxHash = "0x" + f.History.TxHash
	expect(t, "tx hash compared case-insensitively", f)

	f = failedBeforeSigning()
	f.History = &History{Status: "confirmed", Amount: "1000", AssetID: asset}
	expect(t, "failed request, history says confirmed", f, "W5")
	f.History = nil
	expect(t, "failed request, no document", f, "W5")
}

func TestW6(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"no signature":   func(f *Facts) { f.Sig = nil },
		"wrong caller":   func(f *Facts) { f.Sig.CallerCN = "sweeper" },
		"nonce mismatch": func(f *Facts) { f.Sig.Nonce = 8 },
		"no nonce":       func(f *Facts) { f.Req.Nonce = nil },
	} {
		f := confirmedERC20()
		mut(&f)
		vs := expect(t, name, f, "W6")
		if len(vs) == 1 && !vs[0].Immediate {
			t.Errorf("%s: W6 cannot be timing and must be recorded at once", name)
		}
	}
	// failed before it was ever signed: no nonce, no signature, fine
	expect(t, "never signed", failedBeforeSigning())
}

func TestW7(t *testing.T) {
	mutate := func(m func(*Facts)) Facts { f := confirmedERC20(); m(&f); return f }
	expect(t, "tx unknown to the chain", mutate(func(f *Facts) { f.Chain = map[string]ChainTx{} }), "W7")
	expect(t, "tx not mined", mutate(func(f *Facts) { c := f.Chain[txHash]; c.Mined = false; f.Chain[txHash] = c }), "W7")
	expect(t, "tx reverted", mutate(func(f *Facts) { c := f.Chain[txHash]; c.Success = false; f.Chain[txHash] = c }), "W7")
	expect(t, "wrong block", mutate(func(f *Facts) { f.Req.ConfirmedBlock = i64(51) }), "W7")
	expect(t, "wrong recipient", mutate(func(f *Facts) {
		c := f.Chain[txHash]
		c.Input = transferInput("0x00000000000000000000000000000000000000e2", 1000)
		f.Chain[txHash] = c
	}), "W7")
	expect(t, "wrong token amount", mutate(func(f *Facts) {
		c := f.Chain[txHash]
		c.Input = transferInput(dest, 999)
		f.Chain[txHash] = c
	}), "W7")
	expect(t, "sent to another contract", mutate(func(f *Facts) {
		c := f.Chain[txHash]
		c.To = "0x00000000000000000000000000000000000000c1"
		f.Chain[txHash] = c
	}), "W7")
	expect(t, "not a transfer call", mutate(func(f *Facts) {
		c := f.Chain[txHash]
		c.Input = []byte{1, 2, 3}
		f.Chain[txHash] = c
	}), "W7")
	expect(t, "asset unknown", mutate(func(f *Facts) { f.Asset = nil }), "W7")

	n := confirmedNative()
	c := n.Chain[txHash]
	c.Value = bi(1001)
	n.Chain[txHash] = c
	expect(t, "native: wrong value", n, "W7")
	n = confirmedNative()
	c = n.Chain[txHash]
	c.To = "0x00000000000000000000000000000000000000e2"
	n.Chain[txHash] = c
	expect(t, "native: wrong recipient", n, "W7")
	n = confirmedNative()
	n.Req.ToAddress = strings.ToUpper(dest[:2]) + strings.ToUpper(dest[2:])
	expect(t, "addresses compared case-insensitively", n)
}

func TestW8(t *testing.T) {
	f := confirmedERC20()
	f.SharedBy = []string{"44444444-4444-4444-4444-444444444444"}
	vs := expect(t, "another request claims the tx", f, "W8")
	if len(vs) == 1 && !vs[0].Immediate {
		t.Error("W8 cannot be timing")
	}

	f = confirmedERC20()
	f.Req.VersionTxHashes = []string{txHash, otherTx}
	f.Chain[otherTx] = ChainTx{Found: true, Mined: true, Success: true, Block: 51, To: token, Value: bi(0), Input: transferInput(dest, 1000)}
	expect(t, "original and a Fee Bump both mined", f, "W8")

	f = confirmedERC20()
	f.Chain[otherTx] = ChainTx{Found: false} // a replaced version that was never mined: expected
	expect(t, "an unmined Fee Bump version is fine", f)
}

func TestW9(t *testing.T) {
	f := confirmedERC20()
	f.GasFeePurpose = ""
	vs := expect(t, "no Gas Fee", f, "W9")
	if len(vs) == 1 && vs[0].Severity != finding.Warn {
		t.Errorf("W9 is a warn: %+v", vs[0])
	}
	f.GasFeePurpose = PurposeWithdrawalReverted
	expect(t, "Gas Fee with the wrong purpose", f, "W9")

	f.GasFeePurpose = ""
	f.Req.GasPrice = bi(0)
	expect(t, "zero-price chain posts no Gas Fee", f)
}

func TestW10(t *testing.T) {
	mutate := func(m func(*Facts)) Facts { f := failedReverted(); m(&f); return f }
	expect(t, "lock not released", mutate(func(f *Facts) { f.Lock.Status = "active" }), "W4", "W10")
	expect(t, "lock settled", mutate(func(f *Facts) { f.Lock.Status = "settled" }), "W10")
	expect(t, "no Gas Fee", mutate(func(f *Facts) { f.GasFeePurpose = "" }), "W10")
	expect(t, "Gas Fee posted as an ordinary withdrawal", mutate(func(f *Facts) { f.GasFeePurpose = PurposeWithdrawal }), "W10")
	expect(t, "request failed for another reason though the tx reverted", mutate(func(f *Facts) { f.Req.RejectReason = "replaced_or_dropped" }), "W10")
	expect(t, "request says tx_reverted but the chain shows success", mutate(func(f *Facts) {
		c := f.Chain[txHash]
		c.Success = true
		f.Chain[txHash] = c
	}), "W10")
	expect(t, "zero-price chain needs no Gas Fee", mutate(func(f *Facts) { f.GasFeePurpose = ""; f.Req.GasPrice = bi(0) }))
}
