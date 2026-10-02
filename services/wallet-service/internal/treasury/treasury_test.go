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
