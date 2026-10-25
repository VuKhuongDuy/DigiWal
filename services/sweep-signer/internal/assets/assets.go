package assets

import (
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

type AssetType string

const (
	Native AssetType = "native"
	ERC20  AssetType = "erc20"
)

// Asset is what sweep-signer needs to know about an asset id to build a tx.
type Asset struct {
	Type     AssetType
	Contract common.Address // erc20 only
}

// Assets maps asset id -> Asset. It is loaded from the one canonical Asset
// Registry file (infra/assets.yaml, ledger.md) mounted into the container — never
// a per-service copy. Fields sweep-signer doesn't need (symbol, decimals,
// hot_floor, ...) are ignored so other readers can extend the file freely.
type Assets map[string]Asset

type registryFile struct {
	Assets []struct {
		ID              string    `yaml:"id"`
		AssetType       AssetType `yaml:"asset_type"`
		ContractAddress string    `yaml:"contract_address"`
	} `yaml:"assets"`
}

func Load(path string) (Assets, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading asset registry: %w", err)
	}
	var f registryFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing asset registry: %w", err)
	}
	if len(f.Assets) == 0 {
		return nil, fmt.Errorf("asset registry %s lists no assets", path)
	}
	out := Assets{}
	for _, e := range f.Assets {
		id := strings.ToLower(e.ID)
		if id == "" {
			return nil, fmt.Errorf("asset entry without id")
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("duplicate asset id %s", id)
		}
		switch e.AssetType {
		case Native:
			out[id] = Asset{Type: Native}
		case ERC20:
			if !common.IsHexAddress(e.ContractAddress) {
				return nil, fmt.Errorf("asset %s: erc20 needs a valid contract_address", id)
			}
			out[id] = Asset{Type: ERC20, Contract: common.HexToAddress(e.ContractAddress)}
		default:
			return nil, fmt.Errorf("asset %s: unknown asset_type %q", id, e.AssetType)
		}
	}
	return out, nil
}

func (a Assets) Lookup(assetID string) (Asset, bool) {
	v, ok := a[strings.ToLower(assetID)]
	return v, ok
}
