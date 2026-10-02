// Package treasury loads the operator's Treasury file (infra/treasury.yaml, wallet-tiering.md
// invariant #9): the public Warm Wallet address per chain. The Warm seed is never in any
// always-on service, so wallet-service cannot derive this address; the operator declares it.
// Only wallet-service reads the file; other services ask GET /v1/wallet/system-addresses.
package treasury

import (
	"fmt"
	"os"
	"regexp"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

// File is the parsed, validated file: one Warm address per chain id.
type File struct{ warm map[int64]common.Address }

var addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Load reads and validates the file. Any problem is an error: wallet-service must not start
// with a Warm address it cannot trust (a typo would let withdrawals pay out to it, and a later
// Treasury Sweep send Hot funds to it).
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading treasury file: %w", err)
	}
	var f struct {
		Chains []struct {
			ChainID     int64  `yaml:"chain_id"`
			WarmAddress string `yaml:"warm_address"`
		} `yaml:"chains"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing treasury file: %w", err)
	}
	if len(f.Chains) == 0 {
		return nil, fmt.Errorf("treasury file %s lists no chains", path)
	}
	out := &File{warm: map[int64]common.Address{}}
	for i, c := range f.Chains {
		where := fmt.Sprintf("chains[%d] (chain_id %d)", i, c.ChainID)
		if c.ChainID <= 0 {
			return nil, fmt.Errorf("%s: chain_id must be a positive integer", where)
		}
		if _, dup := out.warm[c.ChainID]; dup {
			return nil, fmt.Errorf("%s: duplicate chain_id", where)
		}
		if !addressRe.MatchString(c.WarmAddress) {
			return nil, fmt.Errorf("%s: warm_address %q is not a 0x-prefixed 20-byte hex address", where, c.WarmAddress)
		}
		a := common.HexToAddress(c.WarmAddress)
		if a == (common.Address{}) {
			return nil, fmt.Errorf("%s: warm_address is the zero address", where)
		}
		out.warm[c.ChainID] = a
	}
	return out, nil
}

// WarmFor returns the Warm address for the chain wallet-service runs on, refusing a chain the
// file does not cover and a Warm address equal to the Hot Wallet's (they are separate tiers,
// wallet-tiering.md invariant #1: one seed must never back both).
func (f *File) WarmFor(chainID int64, hot common.Address) (common.Address, error) {
	warm, ok := f.warm[chainID]
	if !ok {
		return common.Address{}, fmt.Errorf("treasury file has no entry for chain %d", chainID)
	}
	if warm == hot {
		return common.Address{}, fmt.Errorf("treasury file: warm_address for chain %d is the Hot Wallet address %s", chainID, hot.Hex())
	}
	return warm, nil
}
