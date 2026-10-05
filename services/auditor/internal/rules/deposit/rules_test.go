package deposit

import (
	"math/big"
	"sort"
	"strings"
	"testing"

	"digiwal/services/auditor/internal/finding"
)

const (
	asset = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	other = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	addr  = "0x00000000000000000000000000000000000000a1"
	hashA = "0xaaa0000000000000000000000000000000000000000000000000000000000001"
	hashB = "0xbbb0000000000000000000000000000000000000000000000000000000000002"
)

func bi(n int64) *big.Int { return big.NewInt(n) }

// good is a fully consistent confirmed, credited deposit of 500.
func good() Facts {
	return Facts{
		Observed: &Observed{AssetID: asset, To: addr, UserID: "u1", Amount: bi(500), Block: 10, BlockHash: hashA, Status: StatusConfirmed},
		Ledger:   &Ledger{ID: "tx1", UserID: "u1", AssetID: asset, Amount: bi(500)},
		Owner:    "u1",
		History:  &History{Status: "confirmed", Amount: "500", AssetID: asset},
		Chain:    OnChain{Found: true, Success: true, Block: 10, BlockHash: hashA, Canonical: hashA, To: addr, Amount: bi(500), AssetID: asset},
	}
}

func flagged() Facts {
	f := good()
	f.Ledger, f.Flagged = nil, true
	return f
}

func expect(t *testing.T, name string, f Facts, want ...string) []finding.Violation {
	t.Helper()
	vs := Evaluate(f)
	var got []string
	for _, v := range vs {
		got = append(got, v.Rule)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: violated %v, want %v (%+v)", name, got, want, vs)
	}
	return vs
}

func TestConsistentDepositsViolateNothing(t *testing.T) {
	expect(t, "credited", good())
	expect(t, "flagged instead of credited", flagged())
	p := good()
	p.Observed.Status, p.Ledger, p.History = "pending", nil, nil
	expect(t, "still pending is not audited", p)
	expect(t, "nothing", Facts{})
}

func TestD1(t *testing.T) {
	f := good()
	f.Ledger = nil
	expect(t, "confirmed but neither credited nor flagged", f, "D1")
	f = good()
	f.Flagged = true
	expect(t, "credited and flagged", f, "D1")
}

func TestD2(t *testing.T) {
	f := good()
	f.Observed, f.History = nil, nil
	f.Chain = OnChain{}
	expect(t, "ledger credit with no observed transfer", f, "D2")
	f = good()
	f.Observed.Status = "pending"
	expect(t, "credit while the observed transfer is still pending", f, "D2")
}

func TestD3(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"ledger credited another user": func(f *Facts) { f.Ledger.UserID = "u2" },
		"observed names another user":  func(f *Facts) { f.Observed.UserID = "u2" },
		"not a deposit address":        func(f *Facts) { f.Owner = "" },
	} {
		f := good()
		mut(&f)
		expect(t, name, f, "D3")
	}
}

func TestD4(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"ledger amount":   func(f *Facts) { f.Ledger.Amount = bi(499) },
		"history amount":  func(f *Facts) { f.History.Amount = "501" },
		"chain amount":    func(f *Facts) { f.Chain.Amount = bi(400) },
		"ledger asset":    func(f *Facts) { f.Ledger.AssetID = other },
		"chain asset":     func(f *Facts) { f.Chain.AssetID = other },
		"observed amount": func(f *Facts) { f.Observed.Amount = bi(1) },
	} {
		f := good()
		mut(&f)
		expect(t, name, f, "D4")
	}
	f := good()
	f.Chain.AssetID = strings.ToUpper(asset)
	expect(t, "asset compared case-insensitively", f)
}

func TestD5(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"not on chain":    func(f *Facts) { f.Chain = OnChain{} },
		"reverted":        func(f *Facts) { f.Chain.Success = false },
		"other block":     func(f *Facts) { f.Chain.Block = 11 },
		"reorged out":     func(f *Facts) { f.Chain.Canonical = hashB },
		"receipt in fork": func(f *Facts) { f.Chain.BlockHash = hashB },
	} {
		f := good()
		mut(&f)
		got := Evaluate(f)
		has := false
		for _, v := range got {
			has = has || v.Rule == "D5"
		}
		if !has {
			t.Errorf("%s: D5 not raised (%+v)", name, got)
		}
	}
}

func TestD6(t *testing.T) {
	for name, mut := range map[string]func(*Facts){
		"no document":   func(f *Facts) { f.History = nil },
		"still pending": func(f *Facts) { f.History.Status = "pending" },
	} {
		f := good()
		mut(&f)
		vs := expect(t, name, f, "D6")
		if len(vs) == 1 && (vs[0].Severity != finding.Warn || vs[0].EscalateTo != finding.Critical || vs[0].Immediate) {
			t.Errorf("%s: D6 is a warn that escalates and waits for a second pass: %+v", name, vs[0])
		}
	}
}

func TestD7(t *testing.T) {
	f := Facts{Owner: "u1", Chain: OnChain{Found: true, Success: true, Block: 10, To: addr, Amount: bi(500), AssetID: asset}}
	expect(t, "missed by the watcher", f, "D7")

	f.Observed = good().Observed
	f.Observed.Status = "pending"
	expect(t, "watcher has it (still pending)", f)

	f = Facts{Owner: "", Chain: OnChain{Found: true, Success: true, Amount: bi(500)}}
	expect(t, "not a deposit address", f)
	f = Facts{Owner: "u1", Chain: OnChain{Found: true, Success: false, Amount: bi(500)}}
	expect(t, "reverted tx moved nothing", f)
	f = Facts{Owner: "u1", Chain: OnChain{Found: true, Success: true, Amount: bi(0)}}
	expect(t, "zero value", f)
}
