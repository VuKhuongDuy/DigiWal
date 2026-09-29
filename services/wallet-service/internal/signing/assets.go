package signing

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

type AssetType string

const (
	AssetNative AssetType = "native"
	AssetERC20  AssetType = "erc20"
)

// Asset is what wallet-service needs to know about an asset_id to build a tx.
type Asset struct {
	Type     AssetType
	Contract common.Address // erc20 only
}

// Assets maps asset_id -> Asset.
//
// ASSUMPTION, flagged for the Solution Architect: the Asset Registry is owned
// by ledger-service (ledger.md) and no design doc says how wallet-service learns
// an asset_id's contract address. Until that is decided this is a small static
// file (ASSETS_FILE) that must mirror the registry — a copy that can drift.
type Assets map[string]Asset

type assetEntry struct {
	AssetID         string    `json:"asset_id"`
	Type            AssetType `json:"type"`
	ContractAddress string    `json:"contract_address,omitempty"`
}

func LoadAssets(path string) (Assets, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading assets file: %w", err)
	}
	var entries []assetEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing assets file: %w", err)
	}
	out := Assets{}
	for _, e := range entries {
		id := strings.ToLower(e.AssetID)
		if id == "" {
			return nil, fmt.Errorf("asset entry without asset_id")
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("duplicate asset_id %s", id)
		}
		switch e.Type {
		case AssetNative:
			out[id] = Asset{Type: AssetNative}
		case AssetERC20:
			if !common.IsHexAddress(e.ContractAddress) {
				return nil, fmt.Errorf("asset %s: erc20 needs a valid contract_address", id)
			}
			out[id] = Asset{Type: AssetERC20, Contract: common.HexToAddress(e.ContractAddress)}
		default:
			return nil, fmt.Errorf("asset %s: unknown type %q", id, e.Type)
		}
	}
	return out, nil
}

func (a Assets) Lookup(assetID string) (Asset, bool) {
	v, ok := a[strings.ToLower(assetID)]
	return v, ok
}
