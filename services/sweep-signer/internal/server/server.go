// Package server is sweep-signer's HTTP+JSON surface: ONE signing endpoint, POST /v1/sign/user-address, on the mTLS
// listener (pinned leaf, CN sweeper only), and /healthz on the plain one. There is no other operation (invariant 15).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"digiwal/services/sweep-signer/internal/authz"
	"digiwal/services/sweep-signer/internal/sweep"
	"digiwal/services/sweep-signer/internal/vault"
)

// Signer is what the endpoint needs.
type Signer interface {
	Sign(ctx context.Context, req sweep.Request) (sweep.Response, error)
}

// Server wires the endpoint.
type Server struct {
	signer Signer
	pins   *authz.Pins
}

// New builds the server. pins may be nil only in handler-level tests that wrap Routes themselves.
func New(signer Signer, pins *authz.Pins) *Server { return &Server{signer: signer, pins: pins} }

// PlainRoutes is the unauthenticated listener: liveness only. It signs nothing.
func (s *Server) PlainRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// MTLSRoutes is the signing listener's mux.
func (s *Server) MTLSRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sign/user-address", authz.Require(authz.Signers, s.pins, http.HandlerFunc(s.handleSign)))
	return mux
}

// Handler is the signing handler without the authz wrapper (tests of the status mapping).
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handleSign) }

func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	var req sweep.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	resp, err := s.signer.Sign(r.Context(), req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, resp)
	case errors.Is(err, sweep.ErrDestinationNotHotWallet), errors.Is(err, sweep.ErrAddressMismatch), errors.Is(err, sweep.ErrNotASweep):
		writeErr(w, http.StatusForbidden, shortMsg(err))
	case errors.Is(err, sweep.ErrUnknownUser):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sweep.ErrBalanceBelowGas):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, sweep.ErrInvalidRequest), errors.Is(err, sweep.ErrUnknownAsset), errors.Is(err, sweep.ErrUnsupportedChain):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sweep.ErrFrozen), errors.Is(err, sweep.ErrFeeCeiling), errors.Is(err, vault.ErrSeedUnavailable):
		// Not "never": the audit may clear after an operator restart, fees fall, the seed gets converted. The caller retries.
		writeErr(w, http.StatusServiceUnavailable, shortMsg(err))
	default:
		log.Printf("signing user-address tx: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// shortMsg names the class of a refusal without echoing internal detail (amounts, derived addresses) to the caller.
func shortMsg(err error) string {
	for _, e := range []error{sweep.ErrDestinationNotHotWallet, sweep.ErrAddressMismatch, sweep.ErrNotASweep,
		sweep.ErrFrozen, sweep.ErrFeeCeiling, vault.ErrSeedUnavailable} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "refused"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
