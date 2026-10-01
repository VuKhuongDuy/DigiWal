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
	// Sign signs for caller (the verified certificate CN), who alone can later reach the key.
	Sign(ctx context.Context, caller string, req signing.Request) (signing.Response, error)
	// HotWalletAddress is the address derived from the hot_wallet seed.
	HotWalletAddress() common.Address
}

// HotKeys is the read/void/replace side of Hot Wallet signing (withdrawal-flow.md invariants
// #21, #22). Every call is scoped to caller's own keys.
type HotKeys interface {
	Lookup(ctx context.Context, caller, key string) (signing.KeyView, error)
	Void(ctx context.Context, caller, key string) (signing.VoidResponse, error)
	Replace(ctx context.Context, caller, key string, req signing.ReplaceRequest) (signing.ReplaceResponse, error)
}

// AddressLookup resolves an address to its owner (GET /v1/addresses/{address}).
type AddressLookup interface {
	UserForAddress(ctx context.Context, address string) (userID, chain string, err error)
}

// UserSigner signs Sweeps (POST /v1/sign/user-address).
type UserSigner interface {
	Sign(ctx context.Context, req signing.UserRequest) (signing.UserResponse, error)
}

// SystemAddresses are the treasury addresses withdrawal-service must know: the Hot Wallet (derived
// here) and the Warm Wallet (declared by the operator in the treasury file).
type SystemAddresses struct {
	ChainID int64
	Hot     common.Address
	Warm    common.Address
}

type Server struct {
	store     *storage.Store
	cache     *storage.Cache
	signer    Signer     // nil until the user_derivation seed is initialized and loaded
	hotSigner HotSigner  // nil until the hot_wallet seed, RPC and assets are configured
	userSign  UserSigner // nil until both seeds, RPC and assets are configured
	addresses AddressLookup
	nonceGaps NonceGaps
	system    *SystemAddresses // nil until the Hot Wallet is configured (then 503)
	hotKeys   HotKeys          // nil until Hot Wallet signing is configured (then 503)
}

// WithHotKeys enables GET /v1/sign/hot-wallet/{key} and POST .../void and .../replace.
func (s *Server) WithHotKeys(h HotKeys) *Server {
	s.hotKeys = h
	return s
}

// WithSystemAddresses enables GET /v1/wallet/system-addresses.
func (s *Server) WithSystemAddresses(a SystemAddresses) *Server {
	s.system = &a
	return s
}

func NewServer(store *storage.Store, cache *storage.Cache, signer Signer, hotSigner HotSigner, userSign UserSigner) *Server {
	srv := &Server{store: store, cache: cache, signer: signer, hotSigner: hotSigner, userSign: userSign, addresses: store}
	if store != nil {
		srv.nonceGaps = store
	}
	return srv
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
	mux.Handle("GET /v1/sign/hot-wallet/{key}", authz.Require(authz.HotWalletKeyReaders, http.HandlerFunc(s.handleHotKeyLookup)))
	mux.Handle("POST /v1/sign/hot-wallet/{key}/void", authz.Require(authz.HotWalletVoiders, http.HandlerFunc(s.handleHotKeyVoid)))
	mux.Handle("POST /v1/sign/hot-wallet/{key}/replace", authz.Require(authz.HotWalletKeyReaders, http.HandlerFunc(s.handleHotKeyReplace)))
	mux.Handle("GET /v1/wallet/hot-address", authz.Require(authz.HotAddressReaders, http.HandlerFunc(s.handleHotAddress)))
	mux.Handle("POST /v1/wallet/nonce-gap", authz.Require(authz.NonceGapReporters, http.HandlerFunc(s.handleNonceGap)))
	mux.Handle("GET /v1/wallet/system-addresses", authz.Require(authz.SystemAddressReaders, http.HandlerFunc(s.handleSystemAddresses)))
	mux.Handle("GET /v1/addresses/{address}", authz.Require(authz.AddressLookupCallers, http.HandlerFunc(s.handleAddressOwner)))
	mux.Handle("POST /v1/sign/user-address", authz.Require(authz.UserAddressSigners, http.HandlerFunc(s.handleSignUserAddress)))
	return mux
}

// handleHotAddress tells sweeper and withdrawal-service the Hot Wallet's own address, so
// neither keeps a second, driftable copy in config (deposit-flow.md invariant #15).
func (s *Server) handleHotAddress(w http.ResponseWriter, _ *http.Request) {
	if s.hotSigner == nil {
		writeErr(w, http.StatusServiceUnavailable, "hot wallet not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"address": s.hotSigner.HotWalletAddress().Hex()})
}

// NonceGaps is the slice of storage.Store the nonce-gap endpoint needs.
type NonceGaps interface {
	FreezeNonceGap(ctx context.Context, chainID int64, fromNonce uint64) (storage.NonceFreeze, error)
}

// handleNonceGap is POST /v1/wallet/nonce-gap {chain_id, from_nonce} (withdrawal-flow.md invariant
// #19): freeze new Hot Wallet nonce allocation from that nonce on, for every caller. Naturally
// idempotent (the lowest reported nonce wins), so the reporter just retries until 200.
func (s *Server) handleNonceGap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChainID   int64   `json:"chain_id"`
		FromNonce *uint64 `json:"from_nonce"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.ChainID <= 0 || req.FromNonce == nil {
		writeErr(w, http.StatusBadRequest, "body must be {chain_id > 0, from_nonce >= 0}")
		return
	}
	f, err := s.nonceGaps.FreezeNonceGap(r.Context(), req.ChainID, *req.FromNonce)
	switch {
	case err == nil:
		log.Printf("ALERT: Nonce Gap reported on chain %d at nonce %d; Hot Wallet signing frozen from nonce %d", req.ChainID, *req.FromNonce, f.FromNonce)
		writeJSON(w, http.StatusOK, map[string]any{"chain_id": f.ChainID, "frozen_from_nonce": f.FromNonce, "frozen_at": f.At.UTC()})
	case errors.Is(err, storage.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no Hot Wallet nonce was ever allocated on this chain")
	case errors.Is(err, storage.ErrNonceNotAllocated):
		writeErr(w, http.StatusUnprocessableEntity, "from_nonce was never allocated")
	default:
		log.Printf("freezing nonce gap: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// handleSystemAddresses is GET /v1/wallet/system-addresses: {chain_id, hot, warm}. 503 until the
// Hot Wallet (and so the chain) is configured — never a partial or zero address.
func (s *Server) handleSystemAddresses(w http.ResponseWriter, _ *http.Request) {
	if s.system == nil {
		writeErr(w, http.StatusServiceUnavailable, "hot wallet not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id": s.system.ChainID, "hot": s.system.Hot.Hex(), "warm": s.system.Warm.Hex(),
	})
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
	resp, err := s.hotSigner.Sign(r.Context(), authz.CallerCN(r.Context()), req)
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
	case errors.Is(err, storage.ErrKeyVoided):
		// The key was voided (invariant #22): nothing is signed and no nonce is allocated.
		writeErr(w, http.StatusGone, "idempotency key was voided")
	default:
		log.Printf("signing hot wallet tx: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) hotKeyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.hotKeys == nil {
		writeErr(w, http.StatusServiceUnavailable, "hot wallet signing not configured")
		return "", false
	}
	key := r.PathValue("key")
	if key == "" || len(key) > 128 {
		writeErr(w, http.StatusBadRequest, "key is required (max 128 chars)")
		return "", false
	}
	return key, true
}

// handleHotKeyLookup is GET /v1/sign/hot-wallet/{key} (invariant #22): the signature and its
// replacements. Another caller's key, or one never signed, is the same 404.
func (s *Server) handleHotKeyLookup(w http.ResponseWriter, r *http.Request) {
	key, ok := s.hotKeyKey(w, r)
	if !ok {
		return
	}
	v, err := s.hotKeys.Lookup(r.Context(), authz.CallerCN(r.Context()), key)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, v)
	case errors.Is(err, storage.ErrNotFound):
		writeErr(w, http.StatusNotFound, "key not signed")
	default:
		log.Printf("looking up hot wallet key: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// handleHotKeyVoid is POST /v1/sign/hot-wallet/{key}/void (invariant #22): voided, or the signature
// that already exists. Idempotent.
func (s *Server) handleHotKeyVoid(w http.ResponseWriter, r *http.Request) {
	key, ok := s.hotKeyKey(w, r)
	if !ok {
		return
	}
	v, err := s.hotKeys.Void(r.Context(), authz.CallerCN(r.Context()), key)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, v)
	case errors.Is(err, storage.ErrNotCallers):
		writeErr(w, http.StatusConflict, "key belongs to another caller")
	case errors.Is(err, signing.ErrInvalidRequest):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("voiding hot wallet key: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// handleHotKeyReplace is POST /v1/sign/hot-wallet/{key}/replace (invariant #21): a Fee Bump. The
// body has no field that could change the destination or the amount.
func (s *Server) handleHotKeyReplace(w http.ResponseWriter, r *http.Request) {
	key, ok := s.hotKeyKey(w, r)
	if !ok {
		return
	}
	var req signing.ReplaceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be {replacement_seq >= 1, max_gas_price?}")
		return
	}
	resp, err := s.hotKeys.Replace(r.Context(), authz.CallerCN(r.Context()), key, req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, resp)
	case errors.Is(err, signing.ErrInvalidRequest):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrNotFound):
		writeErr(w, http.StatusNotFound, "key not signed")
	case errors.Is(err, storage.ErrNotCallers), errors.Is(err, storage.ErrReplacementGap), errors.Is(err, storage.ErrKeyCancelled):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, signing.ErrGasCeilingExceeded):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, storage.ErrNonceGapOpen):
		writeErr(w, http.StatusServiceUnavailable, "nonce gap open: hot wallet signing frozen")
	default:
		log.Printf("replacing hot wallet tx: %v", err)
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
