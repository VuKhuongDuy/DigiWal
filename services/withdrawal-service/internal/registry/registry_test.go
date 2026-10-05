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

func TestHotLimitsAreReadForDisplayOnly(t *testing.T) {
	write := func(extra string) string {
		p := filepath.Join(t.TempDir(), "assets.yaml")
		body := "assets:\n  - id: 11111111-1111-1111-1111-111111111111\n    asset_type: native\n    symbol: ETH\n    decimals: 18\n    min_withdrawal_amount: \"1\"\n" + extra
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r, err := Load(write("    hot_floor: \"30\"\n    hot_ceiling: \"120\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.Get("11111111-1111-1111-1111-111111111111")
	if a.HotFloor.String() != "30" || a.HotCeiling.String() != "120" {
		t.Fatalf("%+v", a)
	}
	// absent: nil, and it is not an error (the limits do not affect any withdrawal)
	r, err = Load(write(""))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := r.Get("11111111-1111-1111-1111-111111111111"); a.HotFloor != nil || a.HotCeiling != nil {
		t.Fatalf("%+v", a)
	}
	for name, extra := range map[string]string{
		"ceiling below floor": "    hot_floor: \"120\"\n    hot_ceiling: \"30\"\n", "not a number": "    hot_floor: \"lots\"\n", "negative": "    hot_floor: \"-1\"\n",
	} {
		if _, err := Load(write(extra)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
