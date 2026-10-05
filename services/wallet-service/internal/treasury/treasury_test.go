package treasury

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const warm = "0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "treasury.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheRepoFileLoads(t *testing.T) {
	f, err := Load("../../../../infra/treasury.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WarmFor(31337, common.Address{1}); err != nil {
		t.Fatalf("Anvil chain: %v", err)
	}
}

func TestWarmFor(t *testing.T) {
	f, err := Load(write(t, "chains:\n  - chain_id: 31337\n    warm_address: \""+strings.ToLower(warm)+"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.WarmFor(31337, common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3"))
	if err != nil || got != common.HexToAddress(warm) {
		t.Fatalf("got %s, %v", got.Hex(), err)
	}
	if _, err := f.WarmFor(11155111, common.Address{1}); err == nil {
		t.Error("a chain the file does not list was accepted")
	}
	if _, err := f.WarmFor(31337, common.HexToAddress(warm)); err == nil {
		t.Error("a Warm address equal to the Hot Wallet was accepted")
	}
}

// Every way the file can be wrong stops startup.
func TestLoadRefusesBadFiles(t *testing.T) {
	entry := "  - chain_id: 31337\n    warm_address: \"" + warm + "\"\n"
	cases := map[string]string{
		"not yaml":        "chains: [",
		"no chains":       "chains: []\n",
		"zero chain id":   strings.Replace("chains:\n"+entry, "31337", "0", 1),
		"duplicate chain": "chains:\n" + entry + entry,
		"short address":   strings.Replace("chains:\n"+entry, warm, "0x1234", 1),
		"no 0x":           strings.Replace("chains:\n"+entry, warm, warm[2:], 1),
		"zero address":    strings.Replace("chains:\n"+entry, warm, "0x0000000000000000000000000000000000000000", 1),
		"missing address": "chains:\n  - chain_id: 31337\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file: loaded")
	}
}

const cold = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

func TestColdFor(t *testing.T) {
	hot := common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3")
	base := "chains:\n  - chain_id: 31337\n    warm_address: \"" + warm + "\"\n"

	// optional: no cold_address is not an error, and not an address
	f, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok, err := f.ColdFor(31337, hot, common.HexToAddress(warm)); err != nil || ok || c != (common.Address{}) {
		t.Fatalf("absent: %s %v %v", c.Hex(), ok, err)
	}

	f, err = Load(write(t, base+"    cold_address: \""+strings.ToLower(cold)+"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := f.ColdFor(31337, hot, common.HexToAddress(warm))
	if err != nil || !ok || c != common.HexToAddress(cold) {
		t.Fatalf("declared: %s %v %v", c.Hex(), ok, err)
	}
	if _, _, err := f.ColdFor(31337, common.HexToAddress(cold), common.HexToAddress(warm)); err == nil {
		t.Error("a Cold address equal to the Hot Wallet was accepted")
	}
	if _, _, err := f.ColdFor(31337, hot, common.HexToAddress(cold)); err == nil {
		t.Error("a Cold address equal to the Warm Wallet was accepted")
	}
	if _, ok, _ := f.ColdFor(11155111, hot, common.HexToAddress(warm)); ok {
		t.Error("a chain the file does not list has no Cold")
	}
}

func TestLoadRefusesBadColdAddress(t *testing.T) {
	base := "chains:\n  - chain_id: 31337\n    warm_address: \"" + warm + "\"\n    cold_address: "
	for name, v := range map[string]string{"short": `"0x1234"`, "no 0x": `"` + cold[2:] + `"`, "zero": `"0x0000000000000000000000000000000000000000"`, "not hex": `"0xZZ0000000000000000000000000000000000000"`} {
		if _, err := Load(write(t, base+v+"\n")); err == nil {
			t.Errorf("%s: a bad cold_address was accepted", name)
		}
	}
}
