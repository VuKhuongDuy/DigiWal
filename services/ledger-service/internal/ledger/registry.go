package ledger

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type AssetType string

const (
	AssetNative AssetType = "native"
	AssetERC20  AssetType = "erc20"
)

// Asset is one Asset Registry entry (ledger.md, "Asset").
type Asset struct {
	ID                  string
	Type                AssetType
	ContractAddress     string // erc20 only
	Symbol              string
	Decimals            int
	HotFloor            *big.Int
	HotCeiling          *big.Int
	MinWithdrawalAmount *big.Int
}

type registryFile struct {
	Assets []struct {
		ID                  string    `yaml:"id"`
		AssetType           AssetType `yaml:"asset_type"`
		ContractAddress     string    `yaml:"contract_address"`
		Symbol              string    `yaml:"symbol"`
		Decimals            *int      `yaml:"decimals"`
		HotFloor            string    `yaml:"hot_floor"`
		HotCeiling          string    `yaml:"hot_ceiling"`
		MinWithdrawalAmount string    `yaml:"min_withdrawal_amount"`
	} `yaml:"assets"`
}

var (
	uuidRe     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	addressRe  = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	maxAmount  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	uintString = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
)

// LoadRegistry reads and strictly validates the canonical Asset Registry file
// (infra/assets.yaml, shared with the other services — ledger.md "Decided: one
// canonical Asset Registry file"). Any problem fails startup with a message naming
// the entry, rather than booting with a half-usable registry. Fields other services
// own are ignored, so the file can be extended freely.
func LoadRegistry(path string) ([]Asset, error) {
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
	seen := map[string]bool{}
	var out []Asset
	for i, e := range f.Assets {
		where := fmt.Sprintf("assets[%d] (%s)", i, e.Symbol)
		id := strings.ToLower(e.ID)
		if !uuidRe.MatchString(id) {
			return nil, fmt.Errorf("%s: id %q is not a uuid", where, e.ID)
		}
		if seen[id] {
			return nil, fmt.Errorf("%s: duplicate id %s", where, id)
		}
		seen[id] = true
		if strings.TrimSpace(e.Symbol) == "" {
			return nil, fmt.Errorf("%s: symbol is required", where)
		}
		if e.Decimals == nil || *e.Decimals < 0 || *e.Decimals > 77 {
			return nil, fmt.Errorf("%s: decimals must be 0..77", where)
		}
		a := Asset{ID: id, Type: e.AssetType, Symbol: e.Symbol, Decimals: *e.Decimals}
		switch e.AssetType {
		case AssetNative:
			if e.ContractAddress != "" {
				return nil, fmt.Errorf("%s: a native asset has no contract_address", where)
			}
		case AssetERC20:
			if !addressRe.MatchString(e.ContractAddress) {
				return nil, fmt.Errorf("%s: erc20 needs a valid contract_address", where)
			}
			a.ContractAddress = e.ContractAddress
		default:
			return nil, fmt.Errorf("%s: asset_type must be native or erc20, got %q", where, e.AssetType)
		}
		var err error
		if a.HotFloor, err = parseAmount(e.HotFloor); err != nil {
			return nil, fmt.Errorf("%s: hot_floor: %w", where, err)
		}
		if a.HotCeiling, err = parseAmount(e.HotCeiling); err != nil {
			return nil, fmt.Errorf("%s: hot_ceiling: %w", where, err)
		}
		if a.MinWithdrawalAmount, err = parseAmount(e.MinWithdrawalAmount); err != nil {
			return nil, fmt.Errorf("%s: min_withdrawal_amount: %w", where, err)
		}
		if a.HotCeiling.Cmp(a.HotFloor) < 0 {
			return nil, fmt.Errorf("%s: hot_ceiling must be >= hot_floor", where)
		}
		out = append(out, a)
	}
	return out, nil
}

// parseAmount reads a non-negative integer in minor units. Amounts are quoted
// strings in YAML so a big value (30 ETH in wei) never passes through a float.
func parseAmount(s string) (*big.Int, error) {
	if !uintString.MatchString(s) {
		return nil, fmt.Errorf("%q must be a non-negative integer in minor units (quote it in YAML)", s)
	}
	n, _ := new(big.Int).SetString(s, 10)
	if n.Cmp(maxAmount) > 0 {
		return nil, fmt.Errorf("%q exceeds uint256", s)
	}
	return n, nil
}

// SyncAssets makes the database mirror the registry: upserts every listed asset,
// makes sure each has its Chain Account, and deactivates assets that left the file
// (never deletes them — accounts and entries still reference them). Idempotent.
func (s *Store) SyncAssets(ctx context.Context, assets []Asset) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		var contract *string
		if a.ContractAddress != "" {
			c := strings.ToLower(a.ContractAddress)
			contract = &c
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount, active)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, TRUE)
			ON CONFLICT (id) DO UPDATE SET
				asset_type = EXCLUDED.asset_type, contract_address = EXCLUDED.contract_address,
				symbol = EXCLUDED.symbol, decimals = EXCLUDED.decimals,
				hot_floor = EXCLUDED.hot_floor, hot_ceiling = EXCLUDED.hot_ceiling,
				min_withdrawal_amount = EXCLUDED.min_withdrawal_amount,
				active = TRUE, updated_at = now()`,
			a.ID, string(a.Type), contract, a.Symbol, a.Decimals,
			a.HotFloor.String(), a.HotCeiling.String(), a.MinWithdrawalAmount.String()); err != nil {
			return fmt.Errorf("upserting asset %s: %w", a.Symbol, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO accounts (asset_id, kind) VALUES ($1, 'chain') ON CONFLICT (asset_id) WHERE kind = 'chain' DO NOTHING`,
			a.ID); err != nil {
			return fmt.Errorf("creating chain account for %s: %w", a.Symbol, err)
		}
		if a.Type == AssetNative { // gas is only ever paid in the native asset (ledger.md invariant #12)
			if _, err := tx.Exec(ctx,
				`INSERT INTO accounts (asset_id, kind) VALUES ($1, 'gas_expense') ON CONFLICT (asset_id) WHERE kind = 'gas_expense' DO NOTHING`,
				a.ID); err != nil {
				return fmt.Errorf("creating gas expense account for %s: %w", a.Symbol, err)
			}
		}
		ids = append(ids, a.ID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE assets SET active = FALSE, updated_at = now() WHERE active AND NOT (id = ANY($1::uuid[]))`, ids); err != nil {
		return fmt.Errorf("deactivating removed assets: %w", err)
	}
	return tx.Commit(ctx)
}
