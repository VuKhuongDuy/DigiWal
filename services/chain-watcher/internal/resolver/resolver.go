// Package resolver turns a Watched Address into the user_id it belongs to (deposit-flow.md,
// "Address Resolution"): Redis first (wallet-service's address -> user_id hash, O(1)), and on
// a miss wallet-service's GET /v1/addresses/{address} over mTLS. Never a direct read of
// wallet-service's Postgres.
//
// The contract that matters (invariant #1): "not ours" is only ever a clean 404. Any other
// failure — Redis down, wallet-service unreachable, a 5xx — is an error the caller must
// retry, never a silent "not ours" that would drop a real deposit.
package resolver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/redis/go-redis/v9"
)

// AddressOwnersKey is the hash wallet-service maintains (agreed key name).
const AddressOwnersKey = "wallet:address_owners"

type Resolver interface {
	// Resolve returns the owner of addr. found=false with a nil error means a definite
	// "not one of our addresses".
	Resolve(ctx context.Context, addr common.Address) (userID string, found bool, err error)
}

// Remote is the Redis + wallet-service implementation.
type Remote struct {
	rdb     *redis.Client
	http    *http.Client
	baseURL string

	// A short-lived in-memory cache of definite 404s. A block on a busy chain holds
	// hundreds of transfers to addresses that are not ours, and each is a Redis miss;
	// without this, every one would be an HTTP call to wallet-service. It is safe because an
	// address only becomes ours when wallet-service issues it, which is before anyone can
	// send to it: a stale "no" cannot hide a deposit for longer than negativeTTL. Only
	// definite 404s are cached, never errors.
	negTTL time.Duration
	mu     sync.Mutex
	neg    map[common.Address]time.Time
	now    func() time.Time
}

// New builds a Remote. negativeTTL of 0 disables the negative cache.
func New(rdb *redis.Client, httpClient *http.Client, walletURL string, negativeTTL time.Duration) *Remote {
	return &Remote{rdb: rdb, http: httpClient, baseURL: strings.TrimRight(walletURL, "/"),
		negTTL: negativeTTL, neg: map[common.Address]time.Time{}, now: time.Now}
}

func (r *Remote) Resolve(ctx context.Context, addr common.Address) (string, bool, error) {
	if r.negativelyCached(addr) {
		return "", false, nil
	}
	v, err := r.rdb.HGet(ctx, AddressOwnersKey, strings.ToLower(addr.Hex())).Result()
	switch {
	case err == nil && v != "":
		return v, true, nil
	case err == nil, errors.Is(err, redis.Nil):
		// a miss: fall through to wallet-service
	default:
		// Redis being down must not mean "not ours"; wallet-service is the source of truth,
		// so ask it instead of guessing.
	}
	return r.lookup(ctx, addr)
}

func (r *Remote) lookup(ctx context.Context, addr common.Address) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/v1/addresses/"+addr.Hex(), nil)
	if err != nil {
		return "", false, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("asking wallet-service who owns %s: %w", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.UserID == "" {
			return "", false, fmt.Errorf("wallet-service returned an unusable owner for %s: %q", addr, body)
		}
		return out.UserID, true, nil
	case http.StatusNotFound:
		r.cacheNegative(addr)
		return "", false, nil
	default:
		return "", false, fmt.Errorf("wallet-service answered %d for %s", resp.StatusCode, addr)
	}
}

// FetchHotAddress asks wallet-service (GET /v1/wallet/hot-address, over mTLS) for the address it
// derived for the Hot Wallet. chain-watcher needs it only to skip Hot Wallet Outflow
// (deposit-flow.md invariant #17); like sweeper it asks once at startup and never takes it from
// config, which could drift from what wallet-service actually derived (invariant #15).
func FetchHotAddress(ctx context.Context, httpClient *http.Client, walletURL string) (common.Address, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(walletURL, "/")+"/v1/wallet/hot-address", nil)
	if err != nil {
		return common.Address{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return common.Address{}, fmt.Errorf("calling wallet-service hot-address: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return common.Address{}, fmt.Errorf("wallet-service hot-address answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !common.IsHexAddress(out.Address) {
		return common.Address{}, fmt.Errorf("wallet-service returned an unusable hot address: %q", body)
	}
	a := common.HexToAddress(out.Address)
	if a == (common.Address{}) {
		return common.Address{}, errors.New("wallet-service returned the zero address as the hot address")
	}
	return a, nil
}

func (r *Remote) negativelyCached(addr common.Address) bool {
	if r.negTTL <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.neg[addr]
	if !ok {
		return false
	}
	if r.now().After(until) {
		delete(r.neg, addr)
		return false
	}
	return true
}

func (r *Remote) cacheNegative(addr common.Address) {
	if r.negTTL <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.neg) > 100_000 { // bound memory; a full reset just costs a few lookups
		r.neg = map[common.Address]time.Time{}
	}
	r.neg[addr] = r.now().Add(r.negTTL)
}

// ClientTLSConfig builds chain-watcher's mTLS client config: its own leaf certificate
// (CN=chain-watcher, which wallet-service authorizes for this one endpoint) and the project
// CA as the only trust root.
func ClientTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}
