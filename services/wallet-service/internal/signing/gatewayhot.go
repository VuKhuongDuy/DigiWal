package signing

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// GatewayDeadline bounds one gateway call: the nonce-row lock is held across it (key-custody-hsm.md 5.2).
const GatewayDeadline = 2 * time.Second

// Circuit breaker (invariant 6a): after 3 consecutive failures it is open for 5 s.
const (
	breakerThreshold = 3
	breakerOpenFor   = 5 * time.Second
)

// breaker fails fast while the gateway is known to be down. A policy refusal is NOT a failure (the
// gateway answered); transport errors, timeouts, 5xx and bad signatures are.
type breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time
	now       func() time.Time
}

func (b *breaker) check() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.now().Before(b.openUntil) {
		return fmt.Errorf("%w: hsm_unavailable (circuit breaker open)", ErrSignerUnavailable)
	}
	return nil
}

// tripped is true from the third consecutive failure until the next success (so it stays true across the probe windows while the
// gateway is down, which is what an alert on "breaker open for a minute" needs).
func (b *breaker) tripped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fails >= breakerThreshold
}

func (b *breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.fails = 0
		return
	}
	b.fails++
	if b.fails >= breakerThreshold {
		// Stays >= threshold until a success, so the one probe allowed after the window re-opens it at once.
		b.openUntil = b.now().Add(breakerOpenFor)
	}
}

// GatewayConfig is the hsm-gateway client's configuration.
type GatewayConfig struct {
	BaseURL    string      // https://hsm-gateway:8443
	TLS        *tls.Config // our leaf (CN wallet-service), the project CA; ServerName is forced to "hsm-gateway"
	CallerCN   string      // the claim sent as caller_cn (audited next to the verified CN, never trusted)
	Timeout    time.Duration
	StartRetry time.Duration // how long NewGatewayHot keeps retrying GET /v1/hot/key at start (0: one attempt)
}

// GatewayTLSConfig loads wallet-service's client certificate and the project CA. The gateway's own
// certificate must chain to that CA and carry the name hsm-gateway.
func GatewayTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file holds no certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13, ServerName: "hsm-gateway"}, nil
}

// GatewayHot is the PKCS#11 backend of the design: it never holds a key, it asks hsm-gateway. The Hot
// address is read from GET /v1/hot/key once at start and pinned. There is no software fallback.
type GatewayHot struct {
	cfg     GatewayConfig
	http    *http.Client
	addr    common.Address
	chainID uint64
	serving bool // the gateway's self-reported state at the pin (false for a gateway that does not say)
	br      breaker
}

// BreakerOpen reports whether the circuit breaker has tripped and no call has succeeded since (exported as a gauge).
func (g *GatewayHot) BreakerOpen() bool { return g.br.tripped() }

// NewGatewayHot connects, fetches the Hot key and checks that the advertised public key hashes to
// the advertised address.
func NewGatewayHot(ctx context.Context, cfg GatewayConfig) (*GatewayHot, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = GatewayDeadline
	}
	g := &GatewayHot{cfg: cfg, br: breaker{now: time.Now}}
	g.http = &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg.TLS, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second},
	}
	deadline := time.Now().Add(cfg.StartRetry)
	for {
		err := g.pin(ctx)
		if err == nil {
			return g, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("pinning the Hot address from hsm-gateway: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (g *GatewayHot) pin(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.BaseURL+"/v1/hot/key", nil)
	if err != nil {
		return err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/hot/key: HTTP %d", resp.StatusCode)
	}
	var k struct {
		Address   string `json:"address"`
		PublicKey string `json:"public_key"`
		ChainID   uint64 `json:"chain_id"`
		Serving   bool   `json:"serving"`
	}
	if err := json.Unmarshal(body, &k); err != nil {
		return fmt.Errorf("GET /v1/hot/key: %w", err)
	}
	if !common.IsHexAddress(k.Address) || k.ChainID == 0 {
		return errors.New("GET /v1/hot/key: malformed answer")
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(k.PublicKey, "0x"))
	if err != nil {
		return fmt.Errorf("GET /v1/hot/key: public_key: %w", err)
	}
	pk, err := crypto.UnmarshalPubkey(pub)
	if err != nil {
		return fmt.Errorf("GET /v1/hot/key: public_key: %w", err)
	}
	if crypto.PubkeyToAddress(*pk) != common.HexToAddress(k.Address) {
		return errors.New("GET /v1/hot/key: public key does not match the address")
	}
	g.addr, g.chainID, g.serving = common.HexToAddress(k.Address), k.ChainID, k.Serving
	return nil
}

// Serving is whether the gateway said, when the Hot address was pinned, that its self-test passed and it is
// signing. The Hot cutover tool refuses to run unless it is true.
func (g *GatewayHot) Serving() bool { return g.serving }

func (g *GatewayHot) HotAddress(context.Context) (common.Address, error) { return g.addr, nil }

// PinnedChainID is the chain the gateway's policy is pinned to.
func (g *GatewayHot) PinnedChainID() uint64 { return g.chainID }

func (g *GatewayHot) Available() error { return g.br.check() }

type gatewayWire struct {
	ChainID              uint64 `json:"chain_id"`
	Nonce                uint64 `json:"nonce"`
	To                   string `json:"to"`
	Value                string `json:"value"`
	Data                 string `json:"data"`
	Gas                  uint64 `json:"gas"`
	MaxFeePerGas         string `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas"`
	Operation            string `json:"operation"`
	IdempotencyKey       string `json:"idempotency_key"`
	CallerCN             string `json:"caller_cn,omitempty"`
}

func unavailable(code string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s: %v", ErrSignerUnavailable, code, cause)
	}
	return fmt.Errorf("%w: %s", ErrSignerUnavailable, code)
}

// SignHotTx sends the Sign Intent and returns the signed transaction. It checks that what came back
// is the transaction that was asked for; the sender check against the pinned address is the caller's.
func (g *GatewayHot) SignHotTx(ctx context.Context, tx *types.Transaction, chainID *big.Int, meta HotMeta) (*types.Transaction, error) {
	if err := g.br.check(); err != nil {
		return nil, err
	}
	if tx.Type() != types.DynamicFeeTxType || tx.To() == nil {
		return nil, fmt.Errorf("%w: only EIP-1559 transfers with a recipient go to the gateway", ErrInvalidRequest)
	}
	wire := gatewayWire{
		ChainID: chainID.Uint64(), Nonce: tx.Nonce(), To: strings.ToLower(tx.To().Hex()), Value: tx.Value().String(),
		Data: "0x" + hex.EncodeToString(tx.Data()), Gas: tx.Gas(), MaxFeePerGas: tx.GasFeeCap().String(),
		MaxPriorityFeePerGas: tx.GasTipCap().String(), Operation: meta.Operation, IdempotencyKey: meta.IdempotencyKey,
		CallerCN: g.cfg.CallerCN,
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.BaseURL+"/v1/hot/sign", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		g.br.record(false)
		return nil, unavailable("hsm_unavailable", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		g.br.record(false)
		return nil, unavailable("hsm_unavailable", err)
	}

	switch {
	case resp.StatusCode == http.StatusForbidden:
		var e struct{ Error, Rule string }
		if json.Unmarshal(body, &e) == nil && e.Error == "policy_refused" {
			g.br.record(true) // the gateway answered: it is up, the request was refused
			return nil, &PolicyRefusedError{Rule: e.Rule}
		}
		// any other 403 (certificate not pinned, CN not allowed) is a configuration failure, not a refusal
		g.br.record(false)
		return nil, unavailable("gateway_forbidden", nil)
	case resp.StatusCode == http.StatusBadRequest:
		// wallet-service validated this request itself, so a 400 is a contract bug, not the caller's
		// fault: 503, and the gateway is up (no breaker strike).
		g.br.record(true)
		return nil, unavailable("gateway_rejected_intent", errors.New(strings.TrimSpace(string(body))))
	case resp.StatusCode != http.StatusOK:
		g.br.record(false)
		var e struct{ Error string }
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return nil, unavailable(e.Error, nil)
	}

	var ok struct {
		SignedRawTx string `json:"signed_raw_tx"`
		From        string `json:"from"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		g.br.record(false)
		return nil, unavailable("bad_response", err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(ok.SignedRawTx, "0x"))
	if err != nil {
		g.br.record(false)
		return nil, unavailable("bad_response", err)
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(raw); err != nil {
		g.br.record(false)
		return nil, unavailable("bad_response", err)
	}
	if err := sameIntent(tx, &signed, chainID); err != nil {
		g.br.record(false)
		return nil, unavailable("bad_response", err)
	}
	g.br.record(true)
	return &signed, nil
}

// sameIntent: the gateway rebuilds the transaction itself; what it signed must be exactly what we asked for.
func sameIntent(want, got *types.Transaction, chainID *big.Int) error {
	switch {
	case got.Type() != types.DynamicFeeTxType, got.ChainId().Cmp(chainID) != 0:
		return errors.New("signed transaction has another type or chain")
	case got.Nonce() != want.Nonce(), got.Gas() != want.Gas():
		return errors.New("signed transaction has another nonce or gas")
	case got.To() == nil || *got.To() != *want.To(), got.Value().Cmp(want.Value()) != 0, !bytes.Equal(got.Data(), want.Data()):
		return errors.New("signed transaction has another recipient, value or data")
	case got.GasFeeCap().Cmp(want.GasFeeCap()) != 0, got.GasTipCap().Cmp(want.GasTipCap()) != 0:
		return errors.New("signed transaction has other fees")
	case len(got.AccessList()) != 0:
		return errors.New("signed transaction has an access list")
	}
	return nil
}
