package ledger

import (
	"os"
	"path/filepath"
	"strings"
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

const goodETH = `
  - id: 11111111-1111-1111-1111-111111111111
    asset_type: native
    symbol: ETH
    decimals: 18
    hot_floor: "30000000000000000000"
    hot_ceiling: "120000000000000000000"
    min_withdrawal_amount: "600000000000000"
`

func TestLoadRegistry(t *testing.T) {
	as, err := LoadRegistry(write(t, "assets:"+goodETH+`
  - id: 22222222-2222-2222-2222-222222222222
    asset_type: erc20
    contract_address: "0x5FbDB2315678afecb367f032d93F642f64180aa3"
    symbol: TT
    decimals: 6
    hot_floor: "0"
    hot_ceiling: "0"
    min_withdrawal_amount: "1"
    some_other_service_field: ignored
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 2 || as[0].HotFloor.String() != "30000000000000000000" || as[1].Decimals != 6 || as[1].Type != AssetERC20 {
		t.Fatalf("%+v", as)
	}
}

// an unquoted integer keeps its exact digits (YAML hands us the literal text),
// so it is accepted; only floats like 3e19 are refused.
func TestLoadRegistryUnquotedBigIntIsExact(t *testing.T) {
	as, err := LoadRegistry(write(t, "assets:"+strings.Replace(goodETH, `"30000000000000000000"`, `30000000000000000000`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if as[0].HotFloor.String() != "30000000000000000000" {
		t.Fatalf("lost precision: %s", as[0].HotFloor)
	}
}

func TestLoadRegistryRejectsBadEntries(t *testing.T) {
	rep := func(from, to string) string { return "assets:" + strings.Replace(goodETH, from, to, 1) }
	cases := map[string]string{
		"empty":            "assets: []",
		"not yaml":         "assets: [",
		"bad uuid":         rep("11111111-1111-1111-1111-111111111111", "eth"),
		"no symbol":        rep("symbol: ETH", ""),
		"no decimals":      rep("decimals: 18", ""),
		"decimals too big": rep("decimals: 18", "decimals: 78"),
		"unknown type":     rep("asset_type: native", "asset_type: nft"),
		"native with address": rep("asset_type: native", `asset_type: native
    contract_address: "0x5FbDB2315678afecb367f032d93F642f64180aa3"`),
		"erc20 no address": rep("asset_type: native", "asset_type: erc20"),
		"missing floor":    rep(`hot_floor: "30000000000000000000"`, ""),
		"float floor":      rep(`hot_floor: "3e19"`+"", "hot_floor: 3e19"),
		"negative min":     rep(`"600000000000000"`, `"-1"`),
		"ceiling < floor":  rep(`"120000000000000000000"`, `"1"`),
		"duplicate id":     "assets:" + goodETH + goodETH,
	}
	// "float floor" needs a literal to replace; build it directly
	cases["float floor"] = "assets:" + strings.Replace(goodETH, `hot_floor: "30000000000000000000"`, `hot_floor: 3e19`, 1)
	for name, body := range cases {
		if _, err := LoadRegistry(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadRegistry("/nonexistent.yaml"); err == nil {
		t.Error("missing file accepted")
	}
}

// the checked-in canonical registry must always satisfy ledger-service
func TestCanonicalRegistryLoads(t *testing.T) {
	as, err := LoadRegistry("../../../../infra/assets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(as) == 0 {
		t.Fatal("no assets")
	}
}
