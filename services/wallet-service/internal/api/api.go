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

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/wallet-service/internal/authz"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

// Signer derives addresses from the in-memory (decrypted) master
// wallet. Only wallet-service ever holds one of these; nothing else in
// the system has access to key material.
type Signer interface {
	AddressAt(index uint32) (string, error)
}

// HotSigner signs Hot Wallet transactions (POST /v1/sign/hot-wallet).
type HotSigner interface {
	Sign(ctx context.Context, req signing.Request) (signing.Response, error)
}

// AddressLookup resolves an address to its owner (GET /v1/addresses/{address}).
type AddressLookup interface {
	UserForAddress(ctx context.Context, address string) (userID, chain string, err error)
}

// UserSigner signs Sweeps (POST /v1/sign/user-address).
type UserSigner interface {
	Sign(ctx context.Context, req signing.UserRequest) (signing.UserResponse, error)
}

type Server struct {
	store     *storage.Store
	cache     *storage.Cache
	signer    Signer     // nil until the user_derivation seed is initialized and loaded
	hotSigner HotSigner  // nil until the hot_wallet seed, RPC and assets are configured
	userSign  UserSigner // nil until both seeds, RPC and assets are configured
	addresses AddressLookup
}

func NewServer(store *storage.Store, cache *storage.Cache, signer Signer, hotSigner HotSigner, userSign UserSigner) *Server {
	return &Server{store: store, cache: cache, signer: signer, hotSigner: hotSigner, userSign: userSign, addresses: store}
}

// Routes is the plain-HTTP listener: /healthz (exempt from client certs,
// service-auth-mtls.md invariant #8) plus the already-shipped
// deposit-address endpoints. Moving those behind the mTLS listener with
// CN=api-gateway is service-auth-mtls.md Open Question #4 — pending Tech
// Lead's answer, so their behavior is unchanged for now.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/users/{userID}/deposit-address", s.handleCreateOrGetDepositAddress)
	mux.HandleFunc("GET /v1/users/{userID}/deposit-address", s.handleCreateOrGetDepositAddress)
	return mux
}

// MTLSRoutes is the mTLS listener's mux: every endpoint here is wrapped in
// authz.Require with its own CN allow-list before any handler logic runs.
// The signing endpoints register here (tasks 0.3 and 0.5).
func (s *Server) MTLSRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sign/hot-wallet", authz.Require(authz.HotWalletSigners, http.HandlerFunc(s.handleSignHotWallet)))
	mux.Handle("GET /v1/addresses/{address}", authz.Require(authz.AddressLookupCallers, http.HandlerFunc(s.handleAddressOwner)))
	mux.Handle("POST /v1/sign/user-address", authz.Require(authz.UserAddressSigners, http.HandlerFunc(s.handleSignUserAddress)))
	return mux
}

// handleAddressOwner is chain-watcher's cache-miss fallback (deposit-flow.md, Address
// Resolution): who owns this deposit address? Read-only. 404 means it is genuinely not one
// of ours; the caller must treat any other failure as "unknown, retry", never as "not ours".
func (s *Server) handleAddressOwner(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("address")
	if !common.IsHexAddress(raw) {
		writeErr(w, http.StatusBadRequest, "address must be a 20-byte hex address")
		return
	}
	// Addresses are stored EIP-55 checksummed; accept any casing.
	userID, chain, err := s.addresses.UserForAddress(r.Context(), common.HexToAddress(raw).Hex())
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"user_id": userID, "chain": chain})
	case errors.Is(err, storage.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not a watched address")
	default:
		log.Printf("looking up owner of %s: %v", raw, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// handleSignUserAddress signs a Sweep: the caller picks whose key signs, but the
// destination is enforced to be the Hot Wallet by the signer, not trusted here.
func (s *Server) handleSignUserAddress(w http.ResponseWriter, r *http.Request) {
	if s.userSign == nil {
		writeErr(w, http.StatusServiceUnavailable, "user-address signing not configured")
		return
	}
	var req signing.UserRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	resp, err := s.userSign.Sign(r.Context(), req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, resp)
	case errors.Is(err, signing.ErrDestinationNotHotWallet):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, signing.ErrUnknownUser):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, signing.ErrBalanceBelowGas):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, signing.ErrInvalidRequest), errors.Is(err, signing.ErrUnknownAsset), errors.Is(err, signing.ErrUnsupportedChain):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("signing user-address tx: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// handleSignHotWallet: the request has no field that could select a key —
// it only ever signs with the Hot Wallet (withdrawal-flow.md invariant #5).
func (s *Server) handleSignHotWallet(w http.ResponseWriter, r *http.Request) {
	if s.hotSigner == nil {
		writeErr(w, http.StatusServiceUnavailable, "hot wallet signing not configured")
		return
	}
	var req signing.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	resp, err := s.hotSigner.Sign(r.Context(), req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, resp)
	case errors.Is(err, signing.ErrInvalidRequest), errors.Is(err, signing.ErrUnknownAsset), errors.Is(err, signing.ErrUnsupportedChain):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrIdempotencyKeyReused):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, signing.ErrGasCeilingExceeded):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, storage.ErrNonceGapOpen):
		// Refuse rather than allocate while a Nonce Gap is open (invariant #7).
		writeErr(w, http.StatusServiceUnavailable, "nonce gap open: hot wallet signing frozen")
	default:
		log.Printf("signing hot wallet tx: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
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
	if err := s.cache.AddDepositAddress(ctx, addr, userID); err != nil {
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
