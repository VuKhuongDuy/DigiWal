package cutover

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"digiwal/services/wallet-service/internal/signing"
)

// RPC is Chain over go-ethereum's ethclient.
type RPC struct{ c *ethclient.Client }

// DialRPC connects to an EVM JSON-RPC endpoint.
func DialRPC(ctx context.Context, url string) (*RPC, error) {
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("dialing EVM RPC: %w", err)
	}
	return &RPC{c: c}, nil
}

func (r *RPC) ChainID(ctx context.Context) (*big.Int, error) { return r.c.ChainID(ctx) }

func (r *RPC) Head(ctx context.Context) (uint64, error) { return r.c.BlockNumber(ctx) }

func (r *RPC) NonceAt(ctx context.Context, a common.Address, block *big.Int) (uint64, error) {
	return r.c.NonceAt(ctx, a, block)
}

func (r *RPC) PendingNonceAt(ctx context.Context, a common.Address) (uint64, error) {
	return r.c.PendingNonceAt(ctx, a)
}

func (r *RPC) BalanceAt(ctx context.Context, a common.Address) (*big.Int, error) {
	return r.c.BalanceAt(ctx, a, nil)
}

func (r *RPC) TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	data := append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(holder.Bytes(), 32)...)
	out, err := r.c.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

func (r *RPC) FeeQuote(ctx context.Context) (*big.Int, *big.Int, error) {
	tip, err := r.c.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}
	head, err := r.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if head.BaseFee == nil {
		return nil, nil, errors.New("chain has no EIP-1559 base fee")
	}
	return tip, head.BaseFee, nil
}

func (r *RPC) EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error) {
	return r.c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: value, Data: data})
}

func (r *RPC) Broadcast(ctx context.Context, raw []byte) error {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return err
	}
	err := r.c.SendTransaction(ctx, &tx)
	if err != nil {
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "already known") || strings.Contains(m, "already imported") || strings.Contains(m, "known transaction") || strings.Contains(m, "already exists") {
			return nil
		}
	}
	return err
}

func (r *RPC) Receipt(ctx context.Context, h common.Hash) (*Receipt, error) {
	rc, err := r.c.TransactionReceipt(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	price := rc.EffectiveGasPrice
	if price == nil {
		price = new(big.Int)
	}
	return &Receipt{Success: rc.Status == types.ReceiptStatusSuccessful, Block: rc.BlockNumber.Uint64(), GasUsed: rc.GasUsed, GasPrice: price}, nil
}

// GatewayFromBackend adapts the wallet-service's hsm-gateway client (which pinned the key at construction) to Gateway.
type GatewayFromBackend struct{ G *signing.GatewayHot }

func (g GatewayFromBackend) Key(ctx context.Context) (GatewayKey, error) {
	a, err := g.G.HotAddress(ctx)
	if err != nil {
		return GatewayKey{}, err
	}
	return GatewayKey{Address: a, ChainID: g.G.PinnedChainID(), Serving: g.G.Serving()}, nil
}

// LegacyFromBackend is the old software Hot key as a Legacy.
type LegacyFromBackend struct {
	B    signing.HotBackend
	Addr common.Address
}

// NewLegacy wraps the software Hot backend.
func NewLegacy(ctx context.Context, b signing.HotBackend) (*LegacyFromBackend, error) {
	a, err := b.HotAddress(ctx)
	if err != nil {
		return nil, err
	}
	return &LegacyFromBackend{B: b, Addr: a}, nil
}

func (l *LegacyFromBackend) Address() common.Address { return l.Addr }

func (l *LegacyFromBackend) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	return l.B.SignHotTx(context.Background(), tx, chainID, signing.HotMeta{Operation: signing.OpTreasurySweep, IdempotencyKey: "hot-cutover"})
}

// LedgerClient posts Gas Fees to ledger-service over mTLS (POST /v1/ledger/gas-fees), as CN=hot-cutover.
type LedgerClient struct {
	BaseURL string
	HTTP    *http.Client
}

// LedgerTLS loads the hot-cutover leaf and the project CA for the ledger call.
func LedgerTLS(certFile, keyFile, caFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the ledger client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file holds no certificate")
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13, ServerName: "ledger-service"}}}, nil
}

// PostGasFee is idempotent on the tx hash (the ledger's key is gas:<chain>:<hash>).
func (c *LedgerClient) PostGasFee(ctx context.Context, g GasFee) error {
	b, _ := json.Marshal(map[string]any{"chain_id": g.ChainID, "tx_hash": g.TxHash, "gas_used": g.GasUsed,
		"effective_gas_price": g.GasPrice.String(), "purpose": g.Purpose})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/ledger/gas-fees", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calling ledger-service gas-fees: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest, http.StatusConflict:
		return fmt.Errorf("%w (%d): %s", ErrLedgerRefused, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("ledger-service gas-fees answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
