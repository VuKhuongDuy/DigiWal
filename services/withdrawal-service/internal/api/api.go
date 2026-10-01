// Package api is withdrawal-service's HTTP surface. The client-facing endpoints are reached only
// through api-gateway, over mTLS as CN=api-gateway, which injects the authenticated user_id
// (integration-contracts.md §2/§3, service-auth-mtls.md matrix).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"regexp"

	"digiwal/services/withdrawal-service/internal/authz"
	"digiwal/services/withdrawal-service/internal/store"
)

// Requests is the slice of store.Store the API needs.
type Requests interface {
	Create(ctx context.Context, r store.NewRequest) (store.Withdrawal, bool, error)
	Get(ctx context.Context, userID, idempotencyKey string) (store.Withdrawal, error)
}

type Server struct{ requests Requests }

func NewServer(r Requests) *Server { return &Server{requests: r} }

// Routes is the plain listener: /healthz only (service-auth-mtls.md invariant #8).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// MTLSRoutes is the mTLS listener. Every route checks the client CN before its handler runs.
func (s *Server) MTLSRoutes() http.Handler {
	mux := http.NewServeMux()
	gw := func(h http.HandlerFunc) http.Handler { return authz.Require(authz.GatewayCallers, h) }
	mux.Handle("POST /v1/withdrawals", gw(s.handleCreate))
	mux.Handle("GET /v1/withdrawals/{idempotency_key}", gw(s.handleGet))
	return mux
}

var (
	// The key is opaque to us; bound it so it can't be used to bloat rows (schema: 1-128 chars).
	keyRe = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	// A positive base-10 integer in minor units, no sign, no leading zeros.
	amountRe = regexp.MustCompile(`^[1-9][0-9]{0,77}$`)
)

type createBody struct {
	UserID         string `json:"user_id"` // injected by api-gateway
	IdempotencyKey string `json:"idempotency_key"`
	AssetID        string `json:"asset_id"`
	Amount         string `json:"amount"`
	ToAddress      string `json:"to_address"`
}

type statusResponse struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Status         string  `json:"status"`
	RejectReason   *string `json:"reject_reason,omitempty"`
	TxHash         *string `json:"tx_hash"`
}

func response(w store.Withdrawal) statusResponse {
	return statusResponse{IdempotencyKey: w.IdempotencyKey, Status: w.Status, RejectReason: w.RejectReason, TxHash: w.TxHash}
}

// handleCreate is POST /v1/withdrawals. Only a body that cannot even be stored is refused
// up front (400); business validation (asset, minimum, whitelist, screening, destination)
// happens after the row exists (task 3.2), so it ends in `rejected` rather than vanishing.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var b createBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch {
	case b.UserID == "":
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	case !keyRe.MatchString(b.IdempotencyKey):
		writeErr(w, http.StatusBadRequest, "idempotency_key must be 1-128 printable ASCII characters")
		return
	case b.AssetID == "" || b.ToAddress == "":
		writeErr(w, http.StatusBadRequest, "asset_id and to_address are required")
		return
	case !amountRe.MatchString(b.Amount):
		writeErr(w, http.StatusBadRequest, "amount must be a positive integer in minor units")
		return
	}
	amount, _ := new(big.Int).SetString(b.Amount, 10)
	req := store.NewRequest{UserID: b.UserID, IdempotencyKey: b.IdempotencyKey, AssetID: b.AssetID, Amount: amount, ToAddress: b.ToAddress}

	wd, created, err := s.requests.Create(r.Context(), req)
	if err != nil {
		log.Printf("creating withdrawal request: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !created {
		// Invariant #11: a duplicate never re-validates, re-reserves or re-signs; it reads the
		// existing row back. A different request under the same key is the caller's bug, not a
		// retry, so it is refused rather than answered with another request's status.
		if !wd.SameRequest(req) {
			writeErr(w, http.StatusConflict, "idempotency_key already used for a different withdrawal")
			return
		}
		writeJSON(w, http.StatusOK, response(wd))
		return
	}
	writeJSON(w, http.StatusAccepted, response(wd))
}

// handleGet is GET /v1/withdrawals/{idempotency_key}?user_id=… — the caller's own request only;
// another user's key is a 404 like a missing one (invariant #20).
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	}
	wd, err := s.requests.Get(r.Context(), userID, r.PathValue("idempotency_key"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case err != nil:
		log.Printf("reading withdrawal request: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, response(wd))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
