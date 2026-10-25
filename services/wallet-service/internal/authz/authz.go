// Package authz enforces service-auth-mtls.md's CN authorization policy:
// a CA-signed client certificate is necessary but never sufficient — its
// Common Name must also be on the specific endpoint's allow-list. The
// check runs in middleware, before any handler (nonce allocation, key
// material, ...) executes (invariant #2), and is never delegated to
// anything upstream (invariant #3).
package authz

import (
	"context"
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
	// CNSweepSigner is sweep-signer (key-custody-hsm.md M3b). It owns Sweep signing now, so wallet-service has no
	// /v1/sign/user-address any more. Its one use of wallet-service: in a DEV-profile build (software Hot Wallet, no
	// HSM) it reads the Hot address here; the production binary takes it from hsm-gateway and never calls this.
	CNSweepSigner = "sweep-signer"
	// CNAuditor is the dev-environment auditor (reconciliation-audit.md): read-only, and allowed exactly one
	// endpoint here, GET /v1/wallet/system-addresses.
	CNAuditor = "auditor"
)

// Policy is the v1 authorization matrix for wallet-service, hardcoded on
// purpose (service-auth-mtls.md Alternatives/Open Question #1): changing who
// may sign is a code change that goes through review.
var (
	// HotWalletSigners may call POST /v1/sign/hot-wallet.
	HotWalletSigners = []string{CNWithdrawalService, CNSweeper}
	// HotWalletKeyReaders may call GET /v1/sign/hot-wallet/{key} and POST .../replace
	// (withdrawal-flow.md invariants #21, #22): each caller only ever reaches its own keys
	// (wallet-service scopes them by the verified CN). sweeper has no database, so it reads its
	// Gas Top-Up keys' replacements from here.
	HotWalletKeyReaders = []string{CNWithdrawalService, CNSweeper}
	// HotWalletVoiders may call POST /v1/sign/hot-wallet/{key}/void: withdrawal-service only —
	// sweeper never releases anything (invariant #22). There is no endpoint that cancels a signed
	// tx (the Nonce Filler is an operator CLI), so no service certificate can cancel a payout.
	HotWalletVoiders = []string{CNWithdrawalService}
	// HotAddressReaders may call GET /v1/wallet/hot-address: the services that need the Hot
	// Wallet's own address (deposit-flow.md, Hot Wallet Address Discovery). chain-watcher reads it
	// only to skip Hot Wallet Outflow (invariant #17); it still cannot reach either /v1/sign/*.
	HotAddressReaders = []string{CNWithdrawalService, CNSweeper, CNChainWatcher, CNSweepSigner}
	// SystemAddressReaders may call GET /v1/wallet/system-addresses ({hot, warm, cold?}): withdrawal-service,
	// to refuse withdrawals and whitelist entries to the Hot/Warm Wallet (withdrawal-flow.md
	// invariant #18) and, later, as its Treasury Sweep destination (wallet-tiering.md invariant #9); and the
	// auditor, to know which addresses are the tiers. The auditor reaches nothing else: not hot-address, not
	// any signing endpoint.
	SystemAddressReaders = []string{CNWithdrawalService, CNAuditor}
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cnKey{}, cn)))
	})
}

type cnKey struct{}

// CallerCN is the verified certificate CN Require stored on the request: the identity wallet-service
// scopes Hot Wallet keys by. "" outside a request that passed Require.
func CallerCN(ctx context.Context) string {
	cn, _ := ctx.Value(cnKey{}).(string)
	return cn
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
