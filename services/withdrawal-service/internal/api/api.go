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
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/authz"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/validate"
)

// Requests is the slice of store.Store the API needs.
type Requests interface {
	Create(ctx context.Context, r store.NewRequest) (store.Withdrawal, bool, error)
	Transition(ctx context.Context, id, from, to string, reason *string) (store.Withdrawal, error)
	Get(ctx context.Context, userID, idempotencyKey string) (store.Withdrawal, error)
}

// Whitelist is the slice of store.Store the whitelist endpoints need.
type Whitelist interface {
	AddWhitelistEntry(ctx context.Context, userID, address string) (store.WhitelistEntry, bool, error)
	RemoveWhitelistEntry(ctx context.Context, userID, id string) error
	ListWhitelist(ctx context.Context, userID string) ([]store.WhitelistEntry, error)
	Now(ctx context.Context) (time.Time, error)
}

// Validator is the validation step (task 3.2): the first failing reason, "" if all pass,
// or an error when no verdict could be reached.
type Validator interface {
	Check(ctx context.Context, w store.Withdrawal) (reason string, err error)
}

// Reserver places the Reservation: ledger-service's Lock (task 3.3, invariant #1).
type Reserver interface {
	Lock(ctx context.Context, key, userID, assetID string, amount *big.Int) error
}

type Server struct {
	reserver  Reserver // nil only in tests that stop at `validated`
	requests  Requests
	whitelist Whitelist
	validator Validator
	// Cooldown is the Whitelist Cooldown (Decided: 24h). Configurable only so e2e tests need not wait.
	cooldown time.Duration
	// destinations is nil until task 3.10 wires the wallet-service checks.
	destinations validate.Destinations
}

type Config struct {
	WhitelistCooldown time.Duration
	Destinations      validate.Destinations
	Reserver          Reserver
}

func NewServer(r Requests, wl Whitelist, v Validator, cfg Config) *Server {
	return &Server{requests: r, whitelist: wl, validator: v, cooldown: cfg.WhitelistCooldown, destinations: cfg.Destinations, reserver: cfg.Reserver}
}

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
	mux.Handle("POST /v1/whitelist", gw(s.handleAddWhitelist))
	mux.Handle("DELETE /v1/whitelist/{id}", gw(s.handleRemoveWhitelist))
	mux.Handle("GET /v1/whitelist", gw(s.handleListWhitelist))
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
// happens after the row exists, so a failure ends in `rejected` rather than vanishing.
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
	s.validateNew(w, r, wd)
}

// validateNew runs validation and then the Reservation in-line, right after the row was created:
// only this handler ever moves a row out of `requested` (invariant #11, Open Questions #9b option B).
//   - a check fails: `rejected` with reject_reason, 422 — nothing was reserved (#10);
//   - no verdict (a dependency failed): the row stays `requested`, 503. That key is spent: a
//     retry with it only reads the row back; the Reaper fails it after the Stuck Request Timeout,
//     and the client retries with a new key (#17, #18);
//   - all checks pass: `validated`, then the ledger Lock (#1):
//     locked → `reserved`, 202 (signing and broadcast continue in the background, task 3.3);
//     not enough Available Balance → `rejected` / insufficient_funds, 422, no Lock exists;
//     ledger unreachable → 503, the row stays `validated` and the Reaper Releases and fails it.
func (s *Server) validateNew(w http.ResponseWriter, r *http.Request, wd store.Withdrawal) {
	reason, err := s.validator.Check(r.Context(), wd)
	if err != nil {
		log.Printf("validating withdrawal %s: %v", wd.ID, err)
		writeUnavailable(w, wd)
		return
	}
	if reason != "" {
		s.advance(w, r, wd, store.StatusRequested, store.StatusRejected, &reason, http.StatusUnprocessableEntity)
		return
	}
	next, ok := s.advance(nil, r, wd, store.StatusRequested, store.StatusValidated, nil, 0)
	if !ok || s.reserver == nil {
		if ok {
			writeJSON(w, http.StatusAccepted, response(next))
		} else {
			s.writeCurrent(w, r, wd)
		}
		return
	}
	err = s.reserver.Lock(r.Context(), next.Key(), next.UserID, next.AssetID, next.Amount)
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		why := RejectInsufficientFunds
		s.advance(w, r, next, store.StatusValidated, store.StatusRejected, &why, http.StatusUnprocessableEntity)
	case err != nil:
		log.Printf("reserving withdrawal %s: %v", wd.ID, err)
		writeUnavailable(w, next)
	default:
		s.advance(w, r, next, store.StatusValidated, store.StatusReserved, nil, http.StatusAccepted)
	}
}

// RejectInsufficientFunds: the ledger refused the Lock for lack of Available Balance (Decided).
const RejectInsufficientFunds = "insufficient_funds"

// advance makes one guarded transition. With a writer it also answers: code on success, the
// row's current status if someone else (the Reaper) moved it first. ok reports success.
func (s *Server) advance(w http.ResponseWriter, r *http.Request, wd store.Withdrawal, from, to string, reason *string, code int) (store.Withdrawal, bool) {
	next, err := s.requests.Transition(r.Context(), wd.ID, from, to, reason)
	if err != nil {
		if !errors.Is(err, store.ErrStateChanged) {
			log.Printf("recording %s -> %s for %s: %v", from, to, wd.ID, err)
		}
		if w != nil {
			s.writeCurrent(w, r, wd)
		}
		return store.Withdrawal{}, false
	}
	if w != nil {
		writeJSON(w, code, response(next))
	}
	return next, true
}

// writeCurrent answers with the row as it is now.
func (s *Server) writeCurrent(w http.ResponseWriter, r *http.Request, wd store.Withdrawal) {
	cur, err := s.requests.Get(r.Context(), wd.UserID, wd.IdempotencyKey)
	if err != nil {
		log.Printf("reading withdrawal %s: %v", wd.ID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, response(cur))
}

func writeUnavailable(w http.ResponseWriter, wd store.Withdrawal) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error":           "the request could not be completed; retry with a new idempotency_key",
		"idempotency_key": wd.IdempotencyKey,
	})
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

var (
	hexAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	uuidRe       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type whitelistResponse struct {
	ID      string    `json:"id"`
	UserID  string    `json:"user_id"`
	Address string    `json:"address"`
	Status  string    `json:"status"`
	AddedAt time.Time `json:"added_at"`
}

// handleAddWhitelist is POST /v1/whitelist {user_id, address}. New entries start `pending`
// (invariant #13); re-adding returns the existing entry with its original added_at (#15).
// 201 for a new entry, 200 for an existing one.
func (s *Server) handleAddWhitelist(w http.ResponseWriter, r *http.Request) {
	var b struct {
		UserID  string `json:"user_id"` // injected by api-gateway
		Address string `json:"address"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if b.UserID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if !hexAddressRe.MatchString(b.Address) {
		writeErr(w, http.StatusBadRequest, "address must be a 0x-prefixed 20-byte hex address")
		return
	}
	addr := common.HexToAddress(b.Address)
	if addr == (common.Address{}) {
		writeErr(w, http.StatusBadRequest, "the zero address cannot be whitelisted")
		return
	}
	// Invariant #18: refused up front so the user doesn't wait out the cooldown for an entry
	// that can never be used. UX only — validation (task 3.2) checks again at request time.
	if s.destinations != nil {
		reason, err := s.destinations.Refuse(r.Context(), addr)
		if err != nil {
			log.Printf("checking whitelist destination %s: %v", addr.Hex(), err)
			writeErr(w, http.StatusServiceUnavailable, "destination check unavailable, retry later")
			return
		}
		if reason != "" {
			writeErr(w, http.StatusUnprocessableEntity, reason)
			return
		}
	}
	e, created, err := s.whitelist.AddWhitelistEntry(r.Context(), b.UserID, addr.Hex())
	if err != nil {
		log.Printf("adding whitelist entry: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now, err := s.whitelist.Now(r.Context())
	if err != nil {
		log.Printf("reading clock: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, whitelistResponse{ID: e.ID, UserID: e.UserID, Address: e.Address, Status: e.Status(now, s.cooldown), AddedAt: e.AddedAt})
}

type whitelistItem struct {
	ID      string    `json:"id"`
	Address string    `json:"address"`
	Status  string    `json:"status"`
	AddedAt time.Time `json:"added_at"`
}

// handleListWhitelist is GET /v1/whitelist?user_id=: the caller's own entries, newest first,
// each with its status computed now. Always a JSON array, [] when there are none.
func (s *Server) handleListWhitelist(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	}
	entries, err := s.whitelist.ListWhitelist(r.Context(), userID)
	if err == nil {
		var now time.Time
		if now, err = s.whitelist.Now(r.Context()); err == nil {
			out := make([]whitelistItem, 0, len(entries))
			for _, e := range entries {
				out = append(out, whitelistItem{ID: e.ID, Address: e.Address, Status: e.Status(now, s.cooldown), AddedAt: e.AddedAt})
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	log.Printf("listing whitelist: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

// handleRemoveWhitelist is DELETE /v1/whitelist/{id}?user_id=: immediate (invariant #13) and
// idempotent — an unknown id, or another user's, is a 204 no-op too, so ids can't be probed.
func (s *Server) handleRemoveWhitelist(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if id := r.PathValue("id"); uuidRe.MatchString(id) {
		if err := s.whitelist.RemoveWhitelistEntry(r.Context(), userID, id); err != nil {
			log.Printf("removing whitelist entry: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
