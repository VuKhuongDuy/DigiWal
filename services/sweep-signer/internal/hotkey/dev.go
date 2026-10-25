//go:build !prod

package hotkey

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// WalletService is the DEV-profile source: wallet-service's GET /v1/wallet/hot-address, for a stack whose Hot Wallet is a
// software key and so has no hsm-gateway (plain `make e2e`, the dev stack before the Hot cutover). It trusts wallet-service
// for the destination, which invariant 15 forbids in production; it is compiled out of a `-tags prod` build, so the
// production binary cannot be pointed at it. The chain id comes from the RPC.
type WalletService struct {
	BaseURL string
	HTTP    *http.Client
	ChainID func(ctx context.Context) (uint64, error)
}

// NewWalletService builds the dev source over a client TLS config that carries the sweep-signer leaf.
func NewWalletService(baseURL string, tlsCfg *tls.Config, chainID func(context.Context) (uint64, error)) (Source, error) {
	cfg := tlsCfg.Clone()
	cfg.ServerName = "" // the URL host (wallet-service) names it
	return &WalletService{BaseURL: strings.TrimRight(baseURL, "/"), ChainID: chainID, HTTP: &http.Client{
		Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}}, nil
}

func (w *WalletService) Name() string {
	return "wallet-service " + w.BaseURL + " (DEV profile: not the HSM, not production)"
}

func (w *WalletService) Pin(ctx context.Context) (Pinned, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.BaseURL+"/v1/wallet/hot-address", nil)
	if err != nil {
		return Pinned{}, err
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return Pinned{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return Pinned{}, fmt.Errorf("GET /v1/wallet/hot-address: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !common.IsHexAddress(out.Address) {
		return Pinned{}, errors.New("GET /v1/wallet/hot-address: malformed answer")
	}
	id, err := w.ChainID(ctx)
	if err != nil {
		return Pinned{}, err
	}
	return Pinned{Address: common.HexToAddress(out.Address), ChainID: id}, nil
}
