// Package registry loads what chain-watcher needs from the one canonical Asset Registry
// (infra/assets.yaml, ledger.md): which asset id is the native coin and which ERC-20 contract
// address maps to which asset id. Everything else in the file (symbol, decimals, hot_floor,
// ...) belongs to other readers and is ignored here. Invariant #2 (deposit-flow.md): only
// registered assets ever produce an event.
package registry

import (
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

type Registry struct {
	// Native is the asset id of the native coin, or "" when the file registers none.
	Native string
	// ERC20 maps a token contract to its asset id.
	ERC20 map[common.Address]string
}

type file struct {
	Assets []struct {
		ID              string `yaml:"id"`
		AssetType       string `yaml:"asset_type"`
		ContractAddress string `yaml:"contract_address"`
	} `yaml:"assets"`
}

// Load parses and validates path. Any problem is an error: the caller keeps its previous
// registry (or refuses to start) rather than watch with a half-understood one.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading asset registry: %w", err)
	}
	var f file
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing asset registry: %w", err)
	}
	if len(f.Assets) == 0 {
		return nil, fmt.Errorf("asset registry %s lists no assets", path)
	}
	r := &Registry{ERC20: map[common.Address]string{}}
	seen := map[string]bool{}
	for _, a := range f.Assets {
		id := strings.ToLower(a.ID)
		if id == "" {
			return nil, fmt.Errorf("asset entry without id")
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate asset id %s", id)
		}
		seen[id] = true
		switch a.AssetType {
		case "native":
			if r.Native != "" {
				return nil, fmt.Errorf("asset registry lists more than one native asset (%s, %s)", r.Native, id)
			}
			r.Native = id
		case "erc20":
			if !common.IsHexAddress(a.ContractAddress) {
				return nil, fmt.Errorf("asset %s: erc20 needs a valid contract_address", id)
			}
			addr := common.HexToAddress(a.ContractAddress)
			if other, dup := r.ERC20[addr]; dup {
				return nil, fmt.Errorf("assets %s and %s share contract %s", other, id, addr)
			}
			r.ERC20[addr] = id
		default:
			return nil, fmt.Errorf("asset %s: unknown asset_type %q", id, a.AssetType)
		}
	}
	return r, nil
}

// Contracts lists the registered ERC-20 contracts, for the log filter.
func (r *Registry) Contracts() []common.Address {
	out := make([]common.Address, 0, len(r.ERC20))
	for a := range r.ERC20 {
		out = append(out, a)
	}
	return out
}
