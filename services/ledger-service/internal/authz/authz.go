// Package authz enforces service-auth-mtls.md's CN authorization policy (copy of wallet-service's; a shared
// package needs a repo-root Docker build context, deferred):
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
	CNAPIGateway        = "api-gateway"
)

// ApplyCallers may call POST /v1/ledger/transactions: withdrawal-service only
// (service-auth-mtls.md invariant #7). Hardcoded on purpose (Open Question #1).
var ApplyCallers = []string{CNWithdrawalService}

// BalanceReaders may call GET /v1/ledger/balances/{user_id}: api-gateway only
// (integration-contracts.md §2/§3, service-auth-mtls.md matrix). Read-only.
var BalanceReaders = []string{CNAPIGateway}

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
