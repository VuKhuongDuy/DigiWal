// Package api is ledger-service's HTTP surface. Every state-changing endpoint is served on
// the mTLS listener and restricted by client certificate CN to withdrawal-service
// (service-auth-mtls.md invariant #7): correction posting and the Lock/Settle/Release trio.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"regexp"

	"digiwal/services/ledger-service/internal/authz"
	"digiwal/services/ledger-service/internal/ledger"
)

// Ledger is the slice of ledger.Store the API needs.
type Ledger interface {
	Apply(ctx context.Context, t ledger.Transaction) (ledger.Status, error)
	Lock(ctx context.Context, r ledger.LockRequest) (ledger.Lock, error)
	Settle(ctx context.Context, key string) (ledger.SettleResult, error)
	Release(ctx context.Context, key string) error
	BalanceSheet(ctx context.Context, userID, assetID string) ([]ledger.AssetBalance, error)
}

type Server struct{ ledger Ledger }

func NewServer(l Ledger) *Server { return &Server{ledger: l} }

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
	guard := func(h http.HandlerFunc) http.Handler { return authz.Require(authz.ApplyCallers, h) }
	mux.Handle("POST /v1/ledger/transactions", guard(s.handleApply))
	mux.Handle("POST /v1/ledger/locks", guard(s.handleLock))
	mux.Handle("POST /v1/ledger/locks/{key}/settle", guard(s.handleSettle))
	mux.Handle("POST /v1/ledger/locks/{key}/release", guard(s.handleRelease))
	// Read-only, for api-gateway's GET /v1/balances.
	mux.Handle("GET /v1/ledger/balances/{user_id}", authz.Require(authz.BalanceReaders, http.HandlerFunc(s.handleBalances)))
	return mux
}

type applyRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	SourceType     string `json:"source_type"`
	Entries        []struct {
		AccountRef      string `json:"account_ref"`
		UserID          string `json:"user_id"`
		AssetID         string `json:"asset_id"`
		Amount          string `json:"amount"`
		CorrectsEntryID string `json:"corrects_entry_id"`
	} `json:"entries"`
}

// handleApply posts a correction. A deposit is never accepted here: deposits enter only
// through the deposits.confirmed consumer, which screens the sender first
// (compliance-screening.md invariant #1). Otherwise a caller holding this endpoint's
// certificate could credit any user directly and skip screening.
//
// For a correction the account and amount of every entry are derived from
// corrects_entry_id (ledger.md invariant #11); whatever the caller sends for them is ignored.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	if !decode(w, r, &req) {
		return
	}
	if ledger.SourceType(req.SourceType) != ledger.SourceCorrection {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_type must be correction on this endpoint"})
		return
	}
	tx := ledger.Transaction{IdempotencyKey: req.IdempotencyKey, Source: ledger.SourceCorrection}
	for _, e := range req.Entries {
		tx.Entries = append(tx.Entries, ledger.Entry{CorrectsEntryID: e.CorrectsEntryID})
	}
	status, err := s.ledger.Apply(r.Context(), tx)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
	case errors.Is(err, ledger.ErrInvalidCorrection):
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid_correction", "reason": err.Error()})
	default:
		writeLedgerErr(w, "apply", req.IdempotencyKey, err)
	}
}

type lockRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	UserID         string `json:"user_id"`
	AssetID        string `json:"asset_id"`
	Amount         string `json:"amount"`
}

// handleLock reserves capacity. The caller names (user_id, asset_id), never an account id.
func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	var req lockRequest
	if !decode(w, r, &req) {
		return
	}
	amount, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "amount must be a decimal string"})
		return
	}
	l, err := s.ledger.Lock(r.Context(), ledger.LockRequest{IdempotencyKey: req.IdempotencyKey, UserID: req.UserID, AssetID: req.AssetID, Amount: amount})
	if err != nil {
		writeLedgerErr(w, "lock", req.IdempotencyKey, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"idempotency_key": l.IdempotencyKey, "status": string(l.Status), "amount": l.Amount.String()})
}

func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	res, err := s.ledger.Settle(r.Context(), key)
	if err != nil {
		writeLedgerErr(w, "settle", key, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"idempotency_key": res.IdempotencyKey, "status": "settled",
		"ledger_transaction_id": res.LedgerTransactionID, "entry_ids": res.EntryIDs})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.ledger.Release(r.Context(), key); err != nil {
		writeLedgerErr(w, "release", key, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"idempotency_key": key, "status": "released"})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON body"})
		return false
	}
	return true
}

// writeLedgerErr maps a ledger error onto the wire contract. A lock that was resolved the
// other way is a 409, never a 200: the caller must not believe money moved (or didn't).
func writeLedgerErr(w http.ResponseWriter, op, key string, err error) {
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"status": "insufficient_funds"})
	case errors.Is(err, ledger.ErrLockNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ledger.ErrIdempotencyKeyReused), errors.Is(err, ledger.ErrLockAlreadySettled), errors.Is(err, ledger.ErrLockAlreadyReleased):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ledger.ErrInvalid), errors.Is(err, ledger.ErrUnbalanced), errors.Is(err, ledger.ErrUnknownAsset):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Printf("%s %q: %v", op, key, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// handleBalances returns the user's Total/Locked/Available per registered asset, all amounts as
// decimal strings. Every registered asset has a row even if the user never touched it.
func (s *Server) handleBalances(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}
	assetID := r.URL.Query().Get("asset_id")
	if assetID != "" && !uuidRe.MatchString(assetID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "asset_id must be a uuid"})
		return
	}
	sheet, err := s.ledger.BalanceSheet(r.Context(), userID, assetID)
	if err != nil {
		log.Printf("reading balances: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	type row struct {
		AssetID          string `json:"asset_id"`
		Symbol           string `json:"symbol"`
		TotalBalance     string `json:"total_balance"`
		LockedBalance    string `json:"locked_balance"`
		AvailableBalance string `json:"available_balance"`
	}
	out := make([]row, len(sheet))
	for i, b := range sheet {
		out[i] = row{b.AssetID, b.Symbol, b.Total.String(), b.Locked.String(), b.Available.String()}
	}
	writeJSON(w, http.StatusOK, out)
}
