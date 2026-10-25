// Package policy is the Signing Policy (key-custody-hsm.md invariant 4): the static rule set every
// Sign Intent must pass before any HSM call. It is loaded from one root-owned file, whose SHA-256
// is logged at start and in every audit entry. There is no hot reload: raising a limit is an edit
// plus a restart, so the hash in the log is always the hash of what was enforced.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

// Defaults for fields the file may omit. Caps and windows have NO default: a missing cap is a
// startup error (fail closed), never "unlimited".
const (
	// DefaultFeeCeilingWei is the existing decided DefaultFeeCeiling (50 gwei).
	DefaultFeeCeilingWei = 50_000_000_000
	DefaultNativeGasCap  = 21_000
	DefaultTokenGasCap   = 100_000
	// DefaultRatePerMinute: accepted withdrawals are "one or two per second" (design 5.2), plus
	// Gas Top-Ups and Treasury Sweeps at far lower rate.
	DefaultRatePerMinute = 120
)

// File is the YAML schema.
type File struct {
	Enabled          *bool       `yaml:"enabled"` // kill switch: false refuses everything
	ChainID          uint64      `yaml:"chain_id"`
	HotAddress       string      `yaml:"hot_address"`
	WarmAddress      string      `yaml:"warm_address"` // Treasury Sweep destination; empty = class disabled
	DeniedRecipients []string    `yaml:"denied_recipients"`
	FeeCeilingWei    string      `yaml:"fee_ceiling_wei"`
	RatePerMinute    int         `yaml:"rate_per_minute"`
	Native           AssetFile   `yaml:"native"`
	Tokens           []AssetFile `yaml:"tokens"`
}

// AssetFile is one allowed asset's limits (wei / minor units, quoted decimal strings).
type AssetFile struct {
	Symbol           string `yaml:"symbol"`
	Contract         string `yaml:"contract"` // tokens only
	GasCap           uint64 `yaml:"gas_cap"`
	MaxPerTx         string `yaml:"max_per_tx"`
	TreasuryMaxPerTx string `yaml:"treasury_max_per_tx"`
	Window1h         string `yaml:"window_1h"`
	Window24h        string `yaml:"window_24h"`
}

// NativeKey is the window/asset key of the chain's native coin.
const NativeKey = "native"

// Asset is a parsed AssetFile.
type Asset struct {
	Key              string // NativeKey or the lower-case 0x contract address
	Symbol           string
	Contract         common.Address
	GasCap           uint64
	MaxPerTx         *big.Int
	TreasuryMaxPerTx *big.Int
	Window1h         *big.Int
	Window24h        *big.Int
}

// Policy is the validated, immutable rule set.
type Policy struct {
	Enabled       bool
	ChainID       uint64
	Hot           common.Address
	Warm          common.Address
	HasWarm       bool
	Denied        map[common.Address]struct{}
	FeeCeiling    *big.Int
	RatePerMinute int
	Native        Asset
	Tokens        map[common.Address]Asset
	// Hash is the hex SHA-256 of the file's exact bytes.
	Hash string
}

// LoadOptions tune the file-permission check.
type LoadOptions struct {
	// RequireRootOwner refuses a file not owned by uid 0 (design: "root-owned policy file").
	// Group/other-writable files are always refused.
	RequireRootOwner bool
}

// LoadFile reads, permission-checks and parses the policy file.
func LoadFile(path string, opts LoadOptions) (*Policy, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("policy file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("policy file %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("policy file %s is group/world-writable (mode %v): refusing to start", path, st.Mode().Perm())
	}
	if opts.RequireRootOwner {
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || sys.Uid != 0 {
			return nil, fmt.Errorf("policy file %s is not owned by root: refusing to start (POLICY_REQUIRE_ROOT_OWNER=false relaxes this for dev only)", path)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy file: %w", err)
	}
	return Parse(b)
}

// Parse validates policy bytes (strict: unknown keys are errors).
func Parse(b []byte) (*Policy, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	sum := sha256.Sum256(b)
	p := &Policy{Hash: hex.EncodeToString(sum[:]), Denied: map[common.Address]struct{}{}, Tokens: map[common.Address]Asset{}}

	if f.Enabled == nil {
		return nil, errors.New("policy: enabled must be set explicitly (true|false)")
	}
	p.Enabled = *f.Enabled
	if f.ChainID == 0 {
		return nil, errors.New("policy: chain_id is required (the single pinned chain)")
	}
	p.ChainID = f.ChainID
	hot, err := addr(f.HotAddress)
	if err != nil || hot == (common.Address{}) {
		return nil, fmt.Errorf("policy: hot_address: must be a non-zero address")
	}
	p.Hot = hot
	if f.WarmAddress != "" {
		w, err := addr(f.WarmAddress)
		if err != nil || w == (common.Address{}) || w == hot {
			return nil, errors.New("policy: warm_address must be a non-zero address different from hot_address")
		}
		p.Warm, p.HasWarm = w, true
	}
	for _, d := range f.DeniedRecipients {
		a, err := addr(d)
		if err != nil {
			return nil, fmt.Errorf("policy: denied_recipients: %q is not an address", d)
		}
		p.Denied[a] = struct{}{}
	}
	p.FeeCeiling = big.NewInt(DefaultFeeCeilingWei)
	if f.FeeCeilingWei != "" {
		v, ok := decimal(f.FeeCeilingWei)
		if !ok || v.Sign() == 0 {
			return nil, errors.New("policy: fee_ceiling_wei must be a positive decimal string")
		}
		p.FeeCeiling = v
	}
	p.RatePerMinute = DefaultRatePerMinute
	if f.RatePerMinute != 0 {
		if f.RatePerMinute < 0 {
			return nil, errors.New("policy: rate_per_minute must be positive")
		}
		p.RatePerMinute = f.RatePerMinute
	}
	nat, err := asset(f.Native, NativeKey, DefaultNativeGasCap, false)
	if err != nil {
		return nil, fmt.Errorf("policy: native: %w", err)
	}
	p.Native = nat
	for _, t := range f.Tokens {
		a, err := asset(t, "", DefaultTokenGasCap, true)
		if err != nil {
			return nil, fmt.Errorf("policy: token %q: %w", t.Symbol, err)
		}
		if _, dup := p.Tokens[a.Contract]; dup {
			return nil, fmt.Errorf("policy: token %s listed twice", a.Contract)
		}
		if a.Contract == hot || (p.HasWarm && a.Contract == p.Warm) {
			return nil, errors.New("policy: a token contract equals the hot or warm address")
		}
		p.Tokens[a.Contract] = a
	}
	return p, nil
}

func asset(f AssetFile, key string, defGas uint64, token bool) (Asset, error) {
	a := Asset{Key: key, Symbol: f.Symbol, GasCap: f.GasCap}
	if a.GasCap == 0 {
		a.GasCap = defGas
	}
	if token {
		c, err := addr(f.Contract)
		if err != nil || c == (common.Address{}) {
			return a, errors.New("contract must be a non-zero address")
		}
		a.Contract = c
		a.Key = strings.ToLower(c.Hex())
	} else if f.Contract != "" {
		return a, errors.New("native asset has no contract")
	}
	for _, x := range []struct {
		name string
		src  string
		dst  **big.Int
	}{{"max_per_tx", f.MaxPerTx, &a.MaxPerTx}, {"treasury_max_per_tx", f.TreasuryMaxPerTx, &a.TreasuryMaxPerTx},
		{"window_1h", f.Window1h, &a.Window1h}, {"window_24h", f.Window24h, &a.Window24h}} {
		v, ok := decimal(x.src)
		if !ok {
			return a, fmt.Errorf("%s is required (a decimal string; 0 means nothing allowed, there is no default)", x.name)
		}
		*x.dst = v
	}
	return a, nil
}

func addr(s string) (common.Address, error) {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return common.Address{}, errors.New("bad address")
	}
	if _, err := hex.DecodeString(s[2:]); err != nil {
		return common.Address{}, err
	}
	return common.HexToAddress(s), nil
}

func decimal(s string) (*big.Int, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}
