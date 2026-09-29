// Package api is ledger-service's HTTP surface. POST /v1/ledger/transactions is the
// only mutating endpoint and is served on the mTLS listener, restricted by client
// certificate CN to withdrawal-service (service-auth-mtls.md invariant #7).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"

	"digiwal/services/ledger-service/internal/authz"
	"digiwal/services/ledger-service/internal/ledger"
)

// Applier is the slice of ledger.Store the API needs.
type Applier interface {
	Apply(ctx context.Context, t ledger.Transaction) (ledger.Status, error)
}

type Server struct{ ledger Applier }

func NewServer(l Applier) *Server { return &Server{ledger: l} }

// Routes is the plain listener: /healthz only, exempt from client certs
// (service-auth-mtls.md invariant #8).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// MTLSRoutes is the mTLS listener's mux; every endpoint is wrapped in authz.Require.
func (s *Server) MTLSRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/ledger/transactions", authz.Require(authz.ApplyCallers, http.HandlerFunc(s.handleApply)))
	return mux
}

type applyRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	SourceType     string `json:"source_type"`
	Entries        []struct {
		AccountRef string `json:"account_ref"`
		UserID     string `json:"user_id"`
		AssetID    string `json:"asset_id"`
		Amount     string `json:"amount"` // signed decimal string, minor units
	} `json:"entries"`
}

// handleApply maps the wire request onto ledger.Apply.
//
// A deposit is never accepted here: deposits enter only through the
// deposits.confirmed consumer (which screens the sender first,
// compliance-screening.md invariant #1). Otherwise a caller with this endpoint's
// certificate could credit any user directly and skip screening.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON body"})
		return
	}
	src := ledger.SourceType(req.SourceType)
	if src != ledger.SourceWithdrawal && src != ledger.SourceCorrection {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_type must be withdrawal or correction on this endpoint"})
		return
	}
	tx := ledger.Transaction{IdempotencyKey: req.IdempotencyKey, Source: src}
	for _, e := range req.Entries {
		amt, ok := new(big.Int).SetString(e.Amount, 10)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "amount must be a signed decimal string"})
			return
		}
		tx.Entries = append(tx.Entries, ledger.Entry{Ref: ledger.AccountRef(e.AccountRef), UserID: e.UserID, AssetID: e.AssetID, Amount: amt})
	}

	status, err := s.ledger.Apply(r.Context(), tx)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"status": "insufficient_funds"})
	case errors.Is(err, ledger.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ledger.ErrInvalid), errors.Is(err, ledger.ErrUnbalanced), errors.Is(err, ledger.ErrUnknownAsset):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Printf("apply %q: %v", req.IdempotencyKey, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
