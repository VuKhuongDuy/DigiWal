// Package authz enforces service-auth-mtls.md's CN authorization policy:
// a CA-signed client certificate is necessary but never sufficient — its
// Common Name must also be on the specific endpoint's allow-list. The
// check runs in middleware, before any handler (nonce allocation, key
// material, ...) executes (invariant #2), and is never delegated to
// anything upstream (invariant #3).
package authz

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// Service names double as certificate CNs (service-auth-mtls.md invariant #6).
const (
	CNWithdrawalService = "withdrawal-service"
	CNSweeper           = "sweeper"
	CNChainWatcher      = "chain-watcher"
)

// Policy is the v1 authorization matrix for wallet-service, hardcoded on
// purpose (service-auth-mtls.md Alternatives/Open Question #1): changing who
// may sign is a code change that goes through review.
var (
	// HotWalletSigners may call POST /v1/sign/hot-wallet.
	HotWalletSigners = []string{CNWithdrawalService, CNSweeper}
	// UserAddressSigners may call POST /v1/sign/user-address — sweeper only
	// (invariant #4): withdrawal-service's cert can never pick a user key.
	UserAddressSigners = []string{CNSweeper}
	// HotAddressReaders may call GET /v1/wallet/hot-address: the services that need the Hot
	// Wallet's own address (deposit-flow.md, Hot Wallet Address Discovery). chain-watcher reads it
	// only to skip Hot Wallet Outflow (invariant #17); it still cannot reach either /v1/sign/*.
	HotAddressReaders = []string{CNWithdrawalService, CNSweeper, CNChainWatcher}
	// SystemAddressReaders may call GET /v1/wallet/system-addresses ({hot, warm}): withdrawal-service
	// only, to refuse withdrawals and whitelist entries to the Hot/Warm Wallet (withdrawal-flow.md
	// invariant #18) and, later, as its Treasury Sweep destination (wallet-tiering.md invariant #9).
	SystemAddressReaders = []string{CNWithdrawalService}
	// NonceGapReporters may call POST /v1/wallet/nonce-gap: withdrawal-service only, the one
	// component that tracks Hot Wallet txs to Withdrawal Confirmation Depth (withdrawal-flow.md
	// invariant #19). There is no endpoint to lift a freeze: only an operator, in the database.
	NonceGapReporters = []string{CNWithdrawalService}
	// AddressLookupCallers may call GET /v1/addresses/{address}: chain-watcher, which cannot
	// resolve a deposit's user_id without it (service-auth-mtls.md matrix), and withdrawal-service,
	// which refuses withdrawals and whitelist entries to one of our own deposit addresses
	// (withdrawal-flow.md invariant #18, Decided). Neither can reach /v1/sign/user-address.
	AddressLookupCallers = []string{CNChainWatcher, CNWithdrawalService}
)

// Require wraps next so only clients whose verified certificate CN is in
// allowed reach it. Anything else gets an opaque 403 — no hint whether the
// request would otherwise have succeeded.
func Require(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, cn := range allowed {
		set[cn] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn, ok := peerCN(r)
		if _, allowedCN := set[cn]; !ok || !allowedCN {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// peerCN returns the CN of the verified client certificate. It reads
// VerifiedChains (chains the TLS stack validated against ClientCAs), never
// the raw, unverified PeerCertificates.
func peerCN(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", false
	}
	return r.TLS.VerifiedChains[0][0].Subject.CommonName, true
}

// ServerTLSConfig builds the mTLS listener's config: our certificate, and
// RequireAndVerifyClientCert against the project CA pool only (invariant #1).
func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
