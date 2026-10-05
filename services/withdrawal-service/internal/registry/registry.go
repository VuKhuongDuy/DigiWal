// Package registry loads the canonical Asset Registry (infra/assets.yaml, ledger.md "Decided: one
// canonical Asset Registry file") for what withdrawal-service needs from it: which assets exist and
// each one's Minimum Withdrawal Amount (withdrawal-flow.md invariant #10). hot_floor and hot_ceiling
// are read only to be shown against the Hot balance (metrics); they never affect a withdrawal.
package registry

import (
	"fmt"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Asset struct {
	ID                  string
	Type                string // native | erc20
	ContractAddress     string // erc20 only
	Symbol              string
	Decimals            int
	MinWithdrawalAmount *big.Int
	// HotFloor and HotCeiling are the Hot Wallet limits (wallet-tiering.md), nil if the registry does not carry them.
	HotFloor, HotCeiling *big.Int
}

type Registry struct{ byID map[string]Asset }

// Get looks an asset up by id (case-insensitive).
func (r *Registry) Get(id string) (Asset, bool) {
	a, ok := r.byID[strings.ToLower(id)]
	return a, ok
}

func (r *Registry) Len() int { return len(r.byID) }

var (
	uuidRe     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	addressRe  = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	uintString = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	maxAmount  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

// Load reads and strictly validates the file. Any problem fails startup, naming the entry,
// rather than booting with a registry that would let a dust withdrawal through.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading asset registry: %w", err)
	}
	var f struct {
		Assets []struct {
			ID                  string `yaml:"id"`
			AssetType           string `yaml:"asset_type"`
			ContractAddress     string `yaml:"contract_address"`
			Symbol              string `yaml:"symbol"`
			Decimals            *int   `yaml:"decimals"`
			MinWithdrawalAmount string `yaml:"min_withdrawal_amount"`
			HotFloor            string `yaml:"hot_floor"`
			HotCeiling          string `yaml:"hot_ceiling"`
		} `yaml:"assets"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing asset registry: %w", err)
	}
	if len(f.Assets) == 0 {
		return nil, fmt.Errorf("asset registry %s lists no assets", path)
	}
	r := &Registry{byID: map[string]Asset{}}
	for i, e := range f.Assets {
		where := fmt.Sprintf("assets[%d] (%s)", i, e.Symbol)
		id := strings.ToLower(e.ID)
		if !uuidRe.MatchString(id) {
			return nil, fmt.Errorf("%s: id %q is not a uuid", where, e.ID)
		}
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("%s: duplicate id %s", where, id)
		}
		if e.Decimals == nil || *e.Decimals < 0 || *e.Decimals > 77 {
			return nil, fmt.Errorf("%s: decimals must be 0..77", where)
		}
		a := Asset{ID: id, Type: e.AssetType, Symbol: e.Symbol, Decimals: *e.Decimals}
		switch e.AssetType {
		case "native":
			if e.ContractAddress != "" {
				return nil, fmt.Errorf("%s: a native asset has no contract_address", where)
			}
		case "erc20":
			if !addressRe.MatchString(e.ContractAddress) {
				return nil, fmt.Errorf("%s: erc20 needs a valid contract_address", where)
			}
			a.ContractAddress = e.ContractAddress
		default:
			return nil, fmt.Errorf("%s: asset_type must be native or erc20, got %q", where, e.AssetType)
		}
		if !uintString.MatchString(e.MinWithdrawalAmount) {
			return nil, fmt.Errorf("%s: min_withdrawal_amount %q must be a non-negative integer in minor units (quote it in YAML)", where, e.MinWithdrawalAmount)
		}
		a.MinWithdrawalAmount, _ = new(big.Int).SetString(e.MinWithdrawalAmount, 10)
		if a.MinWithdrawalAmount.Cmp(maxAmount) > 0 {
			return nil, fmt.Errorf("%s: min_withdrawal_amount exceeds uint256", where)
		}
		for _, f := range []struct {
			name, val string
			dst       **big.Int
		}{{"hot_floor", e.HotFloor, &a.HotFloor}, {"hot_ceiling", e.HotCeiling, &a.HotCeiling}} {
			if f.val == "" {
				continue
			}
			if !uintString.MatchString(f.val) {
				return nil, fmt.Errorf("%s: %s %q must be a non-negative integer in minor units (quote it in YAML)", where, f.name, f.val)
			}
			*f.dst, _ = new(big.Int).SetString(f.val, 10)
		}
		if a.HotFloor != nil && a.HotCeiling != nil && a.HotCeiling.Cmp(a.HotFloor) < 0 {
			return nil, fmt.Errorf("%s: hot_ceiling is below hot_floor", where)
		}
		r.byID[id] = a
	}
	return r, nil
}

// New builds a registry directly (tests).
func New(assets ...Asset) *Registry {
	r := &Registry{byID: map[string]Asset{}}
	for _, a := range assets {
		r.byID[strings.ToLower(a.ID)] = a
	}
	return r
}

// All lists the registered assets, ordered by symbol then id.
func (r *Registry) All() []Asset {
	out := make([]Asset, 0, len(r.byID))
	for _, a := range r.byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].ID < out[j].ID
	})
	return out
}
