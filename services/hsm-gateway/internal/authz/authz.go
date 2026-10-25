// Package authz is the gateway's caller authentication: service-auth-mtls.md's CN allow-list per
// endpoint PLUS leaf-certificate fingerprint pinning (key-custody-hsm.md invariant 25, K15). A
// certificate signed by the project CA is necessary but never sufficient: its exact leaf (SHA-256 of
// the DER) must be in the callers file, so a stolen CA key alone does not mint a caller the gateway
// accepts. Both checks run in the TLS handshake and again in middleware.
package authz

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Caller CNs (service names double as certificate CNs).
const (
	CNWalletService = "wallet-service"
	CNSweepSigner   = "sweep-signer"
	CNKeyAdmin      = "key-admin"
	CNAuditor       = "auditor"
)

// Endpoint allow-lists, hardcoded on purpose (changing who may sign is a reviewed code change).
var (
	HotSigners       = []string{CNWalletService}
	HotKeyReaders    = []string{CNWalletService, CNSweepSigner}
	AuditHeadReaders = []string{CNAuditor, CNKeyAdmin}
	// AttestSigners may ask for an xpub Attestation (key-custody-hsm.md invariant 25: POST /v1/attest is key-admin only).
	AttestSigners = []string{CNKeyAdmin}
	// AttestKeyReaders may read the attestation PUBLIC key (ceremony cross-check; the pin that matters is the file
	// outside the database, not this response).
	AttestKeyReaders = []string{CNKeyAdmin, CNAuditor}

	// Vault (key-custody-hsm.md invariants 17 and 25, M3c). Only sweep-signer may unseal (the role table in internal/vault repeats this
	// per role, and the gateway checks both); key-admin seals, reseals, verifies and administers KEKs; the KEK list is readable by the auditor too.
	VaultUnsealers   = []string{CNSweepSigner}
	VaultAdmins      = []string{CNKeyAdmin} // seal, reseal, verify, kek generate/retire/destroy
	VaultStatusReads = []string{CNKeyAdmin, CNAuditor}
)

// Pins is the set of accepted (CN, leaf fingerprint) pairs.
type Pins struct {
	byFP map[string]string // lower-case hex SHA-256 of the leaf DER -> CN
}

type callersFile struct {
	Callers []struct {
		CN     string `yaml:"cn"`
		SHA256 string `yaml:"sha256"`
	} `yaml:"callers"`
}

// LoadPins reads the callers file. An empty list is an error: a gateway that pins nobody serves nobody.
func LoadPins(path string) (*Pins, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("callers file: %w", err)
	}
	var f callersFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("callers file: %w", err)
	}
	p := &Pins{byFP: map[string]string{}}
	for _, c := range f.Callers {
		fp := strings.ToLower(strings.ReplaceAll(c.SHA256, ":", ""))
		if raw, err := hex.DecodeString(fp); err != nil || len(raw) != 32 || c.CN == "" {
			return nil, fmt.Errorf("callers file: bad entry cn=%q sha256=%q", c.CN, c.SHA256)
		}
		p.byFP[fp] = c.CN
	}
	if len(p.byFP) == 0 {
		return nil, errors.New("callers file lists no callers")
	}
	return p, nil
}

// NewPins builds Pins from certificates (tests, tooling).
func NewPins(certs ...*x509.Certificate) *Pins {
	p := &Pins{byFP: map[string]string{}}
	for _, c := range certs {
		p.byFP[Fingerprint(c.Raw)] = c.Subject.CommonName
	}
	return p
}

// Fingerprint is the lower-case hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Check accepts a leaf only if its fingerprint is pinned AND the certificate's CN equals the CN it was pinned under.
func (p *Pins) Check(leaf *x509.Certificate) (cn string, err error) {
	cn, ok := p.byFP[Fingerprint(leaf.Raw)]
	if !ok {
		return "", errors.New("authz: client certificate is not pinned")
	}
	if leaf.Subject.CommonName != cn {
		return "", errors.New("authz: pinned certificate CN mismatch")
	}
	return cn, nil
}

// ServerTLSConfig builds the listener config: TLS 1.3, client certificate required and verified
// against the project CA, then pinned by fingerprint. Session tickets are off so every connection runs
// the full handshake and so the pin check.
func ServerTLSConfig(certFile, keyFile, caFile string, pins *Pins) (*tls.Config, error) {
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
		Certificates:           []tls.Certificate{cert},
		ClientAuth:             tls.RequireAndVerifyClientCert,
		ClientCAs:              pool,
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("authz: no client certificate")
			}
			_, err := pins.Check(cs.PeerCertificates[0])
			return err
		},
	}, nil
}

type cnKey struct{}

// CallerCN is the verified, pinned CN Require stored on the request ("" outside Require).
func CallerCN(ctx context.Context) string {
	cn, _ := ctx.Value(cnKey{}).(string)
	return cn
}

// Require lets only pinned callers whose CN is in allowed reach next; anything else gets an opaque 403.
func Require(allowed []string, pins *Pins, next http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, cn := range allowed {
		set[cn] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn, ok := verified(r, pins)
		if _, allowedCN := set[cn]; !ok || !allowedCN {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cnKey{}, cn)))
	})
}

// verified requires a chain the TLS stack validated (never the raw PeerCertificates alone) and a pinned leaf.
func verified(r *http.Request, pins *Pins) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", false
	}
	cn, err := pins.Check(r.TLS.VerifiedChains[0][0])
	return cn, err == nil
}
