// Package api is wallet-service's HTTP surface: create/init the master
// wallet (operator-only, meant to be run once) and get-or-create a
// user's deposit address (the actual product feature).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
)

// Signer derives addresses from the in-memory (decrypted) master
// wallet. Only wallet-service ever holds one of these; nothing else in
// the system has access to key material.
type Signer interface {
	AddressAt(index uint32) (string, error)
}

type Server struct {
	store  *storage.Store
	cache  *storage.Cache
	signer Signer // nil until the master wallet is initialized and loaded
}

func NewServer(store *storage.Store, cache *storage.Cache, signer Signer) *Server {
	return &Server{store: store, cache: cache, signer: signer}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/users/{userID}/deposit-address", s.handleCreateOrGetDepositAddress)
	mux.HandleFunc("GET /v1/users/{userID}/deposit-address", s.handleCreateOrGetDepositAddress)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleCreateOrGetDepositAddress returns the user's existing deposit
// address, deriving and persisting a new one on first request. EVM
// addresses are chain-agnostic (same address works on every EVM
// network), so there's exactly one per user, not one per network.
func (s *Server) handleCreateOrGetDepositAddress(w http.ResponseWriter, r *http.Request) {
	if s.signer == nil {
		writeErr(w, http.StatusServiceUnavailable, "master wallet not initialized yet")
		return
	}

	userID := strings.TrimSpace(r.PathValue("userID"))
	if userID == "" {
		writeErr(w, http.StatusBadRequest, "userID is required")
		return
	}
	const chain = "evm"

	ctx := r.Context()
	addr, err := s.store.AddressFor(ctx, userID, chain)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, addressResponse{UserID: userID, Chain: chain, Address: addr})
		return
	case errors.Is(err, storage.ErrNotFound):
		// fall through to create one below
	default:
		log.Printf("looking up address for user %s: %v", userID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if r.Method == http.MethodGet {
		writeErr(w, http.StatusNotFound, "no deposit address yet for this user")
		return
	}

	addr, err = s.store.CreateAddress(ctx, userID, chain, s.signer.AddressAt)
	if err != nil {
		log.Printf("creating address for user %s: %v", userID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.cache.AddDepositAddress(ctx, addr); err != nil {
		// Non-fatal: Postgres already has it, chain-watcher can fall
		// back to a Postgres lookup on a cache miss. Just log it.
		log.Printf("caching address %s: %v (non-fatal)", addr, err)
	}

	writeJSON(w, http.StatusCreated, addressResponse{UserID: userID, Chain: chain, Address: addr})
}

type addressResponse struct {
	UserID  string `json:"user_id"`
	Chain   string `json:"chain"`
	Address string `json:"address"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// LoadOrInitSigner loads the master wallet's decrypted seed into
// memory, or returns storage.ErrNotFound if it hasn't been created yet
// (see cmd/service's -init-master-wallet flag).
func LoadOrInitSigner(_ context.Context, mnemonic string) (Signer, error) {
	return hdwallet.FromMnemonic(mnemonic)
}
