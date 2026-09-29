package signing

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "assets.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAssets(t *testing.T) {
	a, err := LoadAssets(write(t, `
assets:
  - id: 11111111-1111-1111-1111-111111111111
    asset_type: native
    symbol: ETH
    decimals: 18
    hot_floor: "30000000000000000000"   # another service's field: must be ignored
  - id: 22222222-2222-2222-2222-222222222222
    asset_type: erc20
    contract_address: "0x5FbDB2315678afecb367f032d93F642f64180aa3"
`))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := a.Lookup("11111111-1111-1111-1111-111111111111"); !ok || v.Type != AssetNative {
		t.Fatalf("native: %+v %v", v, ok)
	}
	if v, ok := a.Lookup("22222222-2222-2222-2222-222222222222"); !ok || v.Type != AssetERC20 || v.Contract.Hex() != "0x5FbDB2315678afecb367f032d93F642f64180aa3" {
		t.Fatalf("erc20: %+v %v", v, ok)
	}
}

func TestLoadAssetsRejectsBadRegistries(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "assets: []",
		"no id":         "assets:\n  - asset_type: native",
		"duplicate":     "assets:\n  - {id: aa, asset_type: native}\n  - {id: AA, asset_type: native}",
		"erc20 no addr": "assets:\n  - {id: aa, asset_type: erc20}",
		"unknown type":  "assets:\n  - {id: aa, asset_type: nft}",
		"not yaml":      "assets: [",
	} {
		if _, err := LoadAssets(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadAssets("/nonexistent/assets.yaml"); err == nil {
		t.Error("missing file accepted")
	}
}

// the checked-in canonical registry must always load
func TestCanonicalRegistryLoads(t *testing.T) {
	a, err := LoadAssets("../../../../infra/assets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Lookup("11111111-1111-1111-1111-111111111111"); !ok {
		t.Fatal("native ETH missing from infra/assets.yaml")
	}
}
