package obs_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"digiwal/services/withdrawal-service/internal/obs"
	"digiwal/services/withdrawal-service/internal/registry"

	"github.com/ethereum/go-ethereum/common"
)

var (
	hot   = common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3")
	warm  = common.HexToAddress("0xa0Ee7A142d267C1f36714E4a8F75612F20a79720")
	cold  = common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	token = common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3")
)

type chainFake struct {
	native    map[common.Address]*big.Int
	tokens    map[common.Address]*big.Int
	failToken bool
}

func (c chainFake) Balance(_ context.Context, a common.Address) (*big.Int, error) {
	return c.native[a], nil
}
func (c chainFake) TokenBalance(_ context.Context, tk, a common.Address) (*big.Int, error) {
	if c.failToken || tk != token {
		return nil, errors.New("no ERC-20 contract there")
	}
	return c.tokens[a], nil
}

func two() *registry.Registry {
	return registry.New(
		registry.Asset{ID: "e", Type: "native", Symbol: "ETH", Decimals: 18},
		registry.Asset{ID: "t", Type: "erc20", ContractAddress: token.Hex(), Symbol: "TKN", Decimals: 6})
}

func eth(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

func TestWalletBalancesPerTierAndAsset(t *testing.T) {
	c := chainFake{
		native: map[common.Address]*big.Int{hot: eth(10), warm: eth(100), cold: eth(1000)},
		tokens: map[common.Address]*big.Int{hot: big.NewInt(2_500_000), warm: big.NewInt(0), cold: big.NewInt(7_000_000)},
	}
	out := scrape(obs.WalletCollector(c, two(), obs.Wallets{Hot: hot, Warm: warm, Cold: cold}))
	for _, want := range []string{
		`digiwal_wallet_balance{asset="ETH",wallet="hot"} 10`, `digiwal_wallet_balance{asset="ETH",wallet="warm"} 100`, `digiwal_wallet_balance{asset="ETH",wallet="cold"} 1000`,
		`digiwal_wallet_balance{asset="TKN",wallet="hot"} 2.5`, `digiwal_wallet_balance{asset="TKN",wallet="warm"} 0`, `digiwal_wallet_balance{asset="TKN",wallet="cold"} 7`,
		`digiwal_wallet_balance_read_timestamp_seconds{asset="ETH",wallet="hot"}`, `digiwal_collector_up{collector="wallets"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNoColdAddressMeansNoColdSeries(t *testing.T) {
	c := chainFake{native: map[common.Address]*big.Int{hot: eth(1), warm: eth(1)}, tokens: map[common.Address]*big.Int{hot: big.NewInt(1), warm: big.NewInt(1)}}
	out := scrape(obs.WalletCollector(c, two(), obs.Wallets{Hot: hot, Warm: warm}))
	if strings.Contains(out, `wallet="cold"`) || !strings.Contains(out, `collector="wallets"} 1`) {
		t.Fatalf("\n%s", out)
	}
}

// A token contract that is not deployed must not hide the ETH balances (and must not read as 0 TKN).
func TestOneUnreadableBalanceIsOneGap(t *testing.T) {
	c := chainFake{native: map[common.Address]*big.Int{hot: eth(10), warm: eth(100)}, failToken: true}
	out := scrape(obs.WalletCollector(c, two(), obs.Wallets{Hot: hot, Warm: warm}))
	if !strings.Contains(out, `digiwal_wallet_balance{asset="ETH",wallet="hot"} 10`) || strings.Contains(out, `asset="TKN"`) || !strings.Contains(out, `collector="wallets"} 0`) {
		t.Fatalf("\n%s", out)
	}
}
