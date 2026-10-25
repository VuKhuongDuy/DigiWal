// Package server is the HTTP+JSON surface: the mTLS routes (/v1/hot/sign, /v1/hot/key,
// /v1/audit/head) and the plain /healthz and /readyz. There is deliberately no route that takes a
// digest, a hash or a raw transaction, and none that returns key material other than the public key.
package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/hsm-gateway/internal/attest"
	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/gateway"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/intent"
	"digiwal/services/hsm-gateway/internal/vault"
)

// SignDeadline bounds one sign call: the caller (wallet-service) holds a DB row lock across it.
const SignDeadline = 2 * time.Second

// RequestSchemas lists every route's request body type; a contract test asserts none of them has
// a digest/hash/signature field (invariant 3).
var RequestSchemas = map[string]reflect.Type{
	"POST /v1/hot/sign": reflect.TypeOf(intent.Wire{}),
	"POST /v1/attest":   reflect.TypeOf(attest.Request{}),
	// Vault bodies carry stored rows and plaintext to seal, never a digest or a signature; the contract test checks that too.
	"POST /v1/vault/seal":   reflect.TypeOf(vault.SealRequest{}),
	"POST /v1/vault/unseal": reflect.TypeOf(vault.Row{}),
}

// SignResponse is the 200 body of POST /v1/hot/sign.
type SignResponse struct {
	SignedRawTx string `json:"signed_raw_tx"`
	TxHash      string `json:"tx_hash"`
	From        string `json:"from"`
	AuditSeq    uint64 `json:"audit_seq"`
	PolicyHash  string `json:"policy_hash"`
}

// KeyResponse is the 200 body of GET /v1/hot/key.
type KeyResponse struct {
	Address    string `json:"address"`
	PublicKey  string `json:"public_key"`
	ChainID    uint64 `json:"chain_id"`
	PolicyHash string `json:"policy_hash"`
	// Serving is false while the self-test has not passed (or the gateway failed closed): the Hot cutover
	// tool (key-custody-hsm.md 5.5) refuses to start against a gateway that is not serving.
	Serving bool `json:"serving"`
}

// AttestResponse is the 200 body of POST /v1/attest. Signature is hex of the raw 64-byte r||s over
// SHA-256(Statement); the caller verifies it against the attestation public key it pins outside the gateway.
type AttestResponse struct {
	Statement string `json:"statement"`
	Alg       string `json:"alg"`
	Signature string `json:"signature"`
	PublicKey string `json:"public_key"`
	AuditSeq  uint64 `json:"audit_seq"`
}

// AttestKeyResponse is the 200 body of GET /v1/attest/key.
type AttestKeyResponse struct {
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"` // hex, 65-byte uncompressed P-256 point
}

// HeadResponse is the 200 body of GET /v1/audit/head.
type HeadResponse struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}

// Server wires the gateway service to HTTP.
type Server struct {
	svc  *gateway.Service
	pins *authz.Pins
}

// New returns a Server.
func New(svc *gateway.Service, pins *authz.Pins) *Server { return &Server{svc: svc, pins: pins} }

// MTLSRoutes is the handler for the mTLS listener.
func (s *Server) MTLSRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/hot/sign", authz.Require(authz.HotSigners, s.pins, http.HandlerFunc(s.sign)))
	mux.Handle("GET /v1/hot/key", authz.Require(authz.HotKeyReaders, s.pins, http.HandlerFunc(s.key)))
	mux.Handle("GET /v1/audit/head", authz.Require(authz.AuditHeadReaders, s.pins, http.HandlerFunc(s.head)))
	mux.Handle("POST /v1/attest", authz.Require(authz.AttestSigners, s.pins, http.HandlerFunc(s.attest)))
	mux.Handle("GET /v1/attest/key", authz.Require(authz.AttestKeyReaders, s.pins, http.HandlerFunc(s.attestKey)))
	mux.Handle("POST /v1/vault/seal", authz.Require(authz.VaultAdmins, s.pins, http.HandlerFunc(s.vaultSeal)))
	mux.Handle("POST /v1/vault/unseal", authz.Require(authz.VaultUnsealers, s.pins, http.HandlerFunc(s.vaultUnseal)))
	mux.Handle("POST /v1/vault/verify", authz.Require(authz.VaultAdmins, s.pins, http.HandlerFunc(s.vaultVerify)))
	mux.Handle("POST /v1/vault/reseal", authz.Require(authz.VaultAdmins, s.pins, http.HandlerFunc(s.vaultReseal)))
	mux.Handle("GET /v1/vault/keks", authz.Require(authz.VaultStatusReads, s.pins, http.HandlerFunc(s.vaultKEKs)))
	mux.Handle("POST /v1/vault/kek/generate", authz.Require(authz.VaultAdmins, s.pins, s.kekAdmin("generate")))
	mux.Handle("POST /v1/vault/kek/retire", authz.Require(authz.VaultAdmins, s.pins, s.kekAdmin("retire")))
	mux.Handle("POST /v1/vault/kek/destroy", authz.Require(authz.VaultAdmins, s.pins, s.kekAdmin("destroy")))
	return metrics.ReportRoute(mux)
}

// PlainRoutes is the handler for the plain listener: liveness and readiness only.
func (s *Server) PlainRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.svc.Ready() {
			writeJSON(w, 200, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, 503, map[string]string{"status": "not_ready", "signing": "unavailable"})
	})
	return mux
}

func (s *Server) sign(w http.ResponseWriter, r *http.Request) {
	in, err := intent.Decode(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	res, err := s.svc.Sign(ctx, authz.CallerCN(r.Context()), in)
	if err != nil {
		writeSignError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SignResponse{
		SignedRawTx: "0x" + hex.EncodeToString(res.RawTx), TxHash: res.TxHash.Hex(), From: res.From.Hex(),
		AuditSeq: res.AuditSeq, PolicyHash: res.PolicyHash,
	})
}

func writeSignError(w http.ResponseWriter, err error) {
	var pr *gateway.PolicyRefused
	var he *hsm.Error
	switch {
	case errors.As(err, &pr):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy_refused", "rule": pr.Rule})
	case errors.Is(err, gateway.ErrNotServing):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_serving"})
	case errors.Is(err, gateway.ErrAudit):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit_unavailable"})
	case errors.Is(err, gateway.ErrRecovery):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "signature_verification_failed"})
	case errors.As(err, &he):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": he.Code})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": hsm.CodeUnavailable})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func (s *Server) key(w http.ResponseWriter, _ *http.Request) {
	pol := s.svc.Engine().Policy()
	writeJSON(w, http.StatusOK, KeyResponse{
		Address: s.svc.Hot().Hex(), PublicKey: "0x" + hex.EncodeToString(s.svc.PublicKey()),
		ChainID: pol.ChainID, PolicyHash: pol.Hash, Serving: s.svc.Ready(),
	})
}

func (s *Server) attest(w http.ResponseWriter, r *http.Request) {
	q, derr := attest.Decode(http.MaxBytesReader(w, r.Body, 4<<10))
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	res, err := s.svc.Attest(ctx, authz.CallerCN(r.Context()), q, derr)
	var he *hsm.Error
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, AttestResponse{Statement: res.Statement, Alg: attest.Alg,
			Signature: hex.EncodeToString(res.Signature), PublicKey: hex.EncodeToString(res.PublicKey), AuditSeq: res.AuditSeq})
	case errors.Is(err, gateway.ErrAttestUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "attest_unavailable"})
	case errors.Is(err, attest.ErrShape):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "attest_refused", "detail": err.Error()})
	case errors.Is(err, gateway.ErrAudit):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit_unavailable"})
	case errors.As(err, &he):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": he.Code})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": hsm.CodeUnavailable})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func (s *Server) attestKey(w http.ResponseWriter, _ *http.Request) {
	pk := s.svc.AttestPublicKey()
	if pk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "attest_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, AttestKeyResponse{Alg: attest.Alg, PublicKey: hex.EncodeToString(pk)})
}

func (s *Server) head(w http.ResponseWriter, _ *http.Request) {
	seq, h := s.svc.AuditHead()
	writeJSON(w, http.StatusOK, HeadResponse{Seq: seq, Hash: h})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
