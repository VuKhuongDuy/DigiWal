package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "assets.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRealRegistryShape(t *testing.T) {
	// The canonical file also carries fields other readers need; this loader must ignore them.
	r, err := Load(write(t, `
assets:
  - id: 11111111-1111-1111-1111-111111111111
    asset_type: native
    symbol: ETH
    decimals: 18
    hot_floor: "30000000000000000000"
  - id: 22222222-2222-2222-2222-222222222222
    asset_type: erc20
    contract_address: "0x5FbDB2315678afecb367f032d93F642f64180aa3"
    symbol: TT
`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Native != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("native = %q", r.Native)
	}
	if got := r.ERC20[common.HexToAddress("0x5fbdb2315678afecb367f032d93f642f64180aa3")]; got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("erc20 lookup (case-insensitive) = %q", got)
	}
	if len(r.Contracts()) != 1 {
		t.Fatalf("contracts = %v", r.Contracts())
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"empty":         "assets: []",
		"two natives":   "assets:\n - {id: a, asset_type: native}\n - {id: b, asset_type: native}",
		"duplicate id":  "assets:\n - {id: a, asset_type: native}\n - {id: A, asset_type: erc20, contract_address: '0x5FbDB2315678afecb367f032d93F642f64180aa3'}",
		"erc20 no addr": "assets:\n - {id: a, asset_type: erc20}",
		"bad type":      "assets:\n - {id: a, asset_type: nft}",
		"same contract": "assets:\n - {id: a, asset_type: erc20, contract_address: '0x5FbDB2315678afecb367f032d93F642f64180aa3'}\n - {id: b, asset_type: erc20, contract_address: '0x5fbdb2315678afecb367f032d93f642f64180aa3'}",
		"no id":         "assets:\n - {asset_type: native}",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "reading") {
		t.Errorf("missing file: %v", err)
	}
}
