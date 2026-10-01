package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The real files must load: the canonical registry and the e2e one (with an ERC-20).
func TestLoadsTheRepoRegistries(t *testing.T) {
	for _, p := range []string{"../../../../infra/assets.yaml", "../../../../e2e/assets.yaml"} {
		r, err := Load(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		eth, ok := r.Get("11111111-1111-1111-1111-111111111111")
		if !ok || eth.Type != "native" || eth.MinWithdrawalAmount.String() != "600000000000000" {
			t.Fatalf("%s: ETH = %+v", p, eth)
		}
	}
}

func TestRejectsBadFiles(t *testing.T) {
	entry := `
  - id: 11111111-1111-1111-1111-111111111111
    asset_type: native
    symbol: ETH
    decimals: 18
    min_withdrawal_amount: "600000000000000"`
	cases := map[string]string{
		"empty":            "assets: []",
		"bad id":           strings.Replace("assets:"+entry, "11111111-1111-1111-1111-111111111111", "eth", 1),
		"duplicate":        "assets:" + entry + entry,
		"no minimum":       strings.Replace("assets:"+entry, `min_withdrawal_amount: "600000000000000"`, "", 1),
		"unquoted minimum": strings.Replace("assets:"+entry, `"600000000000000"`, "6.0e14", 1),
		"bad type":         strings.Replace("assets:"+entry, "native", "btc", 1),
		"no decimals":      strings.Replace("assets:"+entry, "decimals: 18", "", 1),
		"erc20 no address": strings.Replace("assets:"+entry, "native", "erc20", 1),
	}
	for name, body := range cases {
		p := filepath.Join(t.TempDir(), "a.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
