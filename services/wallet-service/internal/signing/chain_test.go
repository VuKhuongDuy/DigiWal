package signing

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

// simChain adapts go-ethereum's in-process simulated EVM to Chain.
type simChain struct{ c simulated.Client }

func (s simChain) ChainID(ctx context.Context) (*big.Int, error) { return s.c.ChainID(ctx) }
func (s simChain) PendingNonceAt(ctx context.Context, a common.Address) (uint64, error) {
	return s.c.PendingNonceAt(ctx, a)
}
func (s simChain) BalanceAt(ctx context.Context, a common.Address) (*big.Int, error) {
	return s.c.BalanceAt(ctx, a, nil)
}
func (s simChain) FeeQuote(ctx context.Context) (*big.Int, *big.Int, error) {
	tip, err := s.c.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}
	head, err := s.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	return tip, head.BaseFee, nil
}
func (s simChain) EstimateGas(ctx context.Context, from, to common.Address, v *big.Int, d []byte) (uint64, error) {
	return s.c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: v, Data: d})
}

// A real EVM (go-ethereum's simulated backend) must accept and mine what we
// sign: right chain id, nonce sequence, fee fields, funded sender.
func TestSign_TransactionsAreAcceptedAndMinedByARealEVM(t *testing.T) {
	hot, err := hdwallet.FromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	hotAddr, _ := hot.AddressAt(hdwallet.HotWalletIndex)
	from := common.HexToAddress(hotAddr)

	backend := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	client := backend.Client()
	id, _ := client.ChainID(context.Background())

	store, err := storage.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	s, err := New(context.Background(), store, hot, simChain{client}, Assets{nativeID: {Type: AssetNative}})
	if err != nil {
		t.Fatal(err)
	}

	to := common.HexToAddress(dest)
	for i, key := range []string{"a", "b", "c"} {
		req := Request{ToAddress: dest, AssetID: nativeID, Amount: "1000000000000000", ChainID: id.Int64(), IdempotencyKey: key}
		resp, err := s.Sign(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Nonce != uint64(i) {
			t.Fatalf("nonce %d, want %d", resp.Nonce, i)
		}
		tx := decode(t, resp)
		if err := client.SendTransaction(context.Background(), tx); err != nil {
			t.Fatalf("EVM rejected tx %d: %v", i, err)
		}
	}
	backend.Commit()

	bal, _ := client.BalanceAt(context.Background(), to, nil)
	if want := big.NewInt(3e15); bal.Cmp(want) != 0 {
		t.Fatalf("recipient balance %s, want %s", bal, want)
	}
	if n, _ := client.NonceAt(context.Background(), from, nil); n != 3 {
		t.Fatalf("on-chain nonce %d, want 3", n)
	}
}
