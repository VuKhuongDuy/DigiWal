// Package api is api-gateway's client-facing surface. Its one job (identity-auth.md): no
// request reaches any other service without a user_id resolved from a valid API key, and that
// user_id is never read from the client.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"digiwal/services/api-gateway/internal/store"
)

type ctxKey struct{}

// UserID returns the authenticated user of a request that passed the auth middleware.
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

type Authenticator interface {
	Authenticate(ctx context.Context, apiKey string) (string, error)
}

type UserCreator interface {
	CreateUser(ctx context.Context) (userID, apiKey string, err error)
}

// Wallet is the slice of wallet-service the gateway calls.
type Wallet interface {
	// DepositAddress returns the user's deposit address, creating it on first use.
	DepositAddress(ctx context.Context, userID string) (status int, body []byte, err error)
}

// Ledger is the slice of ledger-service the gateway calls.
type Ledger interface {
	Balances(ctx context.Context, userID, assetID string) (status int, body []byte, err error)
}

type Server struct {
	Auth   Authenticator
	Users  UserCreator
	Wallet Wallet
	Ledger Ledger
}

// Routes: POST /users is the only unauthenticated route (invariant #3). Everything else,
// including a path that does not exist, is authenticated first — so an unauthenticated
// client learns nothing about which routes exist.
func (s *Server) Routes() http.Handler {
	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/deposit-address", s.handleDepositAddress)
	authed.HandleFunc("GET /v1/balances", s.handleBalances)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	root.HandleFunc("POST /users", s.handleCreateUser)
	root.Handle("/", s.requireAuth(authed))
	return root
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearer(r)
		if !ok {
			unauthorized(w)
			return
		}
		userID, err := s.Auth.Authenticate(r.Context(), key)
		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, userID)))
		case errors.Is(err, store.ErrInvalidKey):
			unauthorized(w)
		default:
			// A lookup failure is not "unauthorized" — the caller may be perfectly valid.
			log.Printf("authenticating: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		}
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, key, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(key) == "" || strings.ContainsAny(key, " \t") {
		return "", false
	}
	return key, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// handleCreateUser mints a user and its API key, shown once (invariants #1, #2). The body is
// ignored: there is nothing a client may choose here, least of all the user_id.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 1<<10))
	userID, apiKey, err := s.Users.CreateUser(r.Context())
	if err != nil {
		log.Printf("creating user: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]string{"user_id": userID, "api_key": apiKey})
}

// handleDepositAddress serves the authenticated user's own address. The user id comes from the
// context only; nothing in the request (path, query, header, body) can name another user.
func (s *Server) handleDepositAddress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, body, err := s.Wallet.DepositAddress(ctx, UserID(r.Context()))
	if err != nil {
		log.Printf("deposit address for %s: %v", UserID(r.Context()), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleBalances returns the authenticated user's Total/Locked/Available per asset, straight
// from ledger-service. The only client-controlled input is the optional asset_id filter; the
// user id comes from the context alone.
func (s *Server) handleBalances(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, body, err := s.Ledger.Balances(ctx, UserID(r.Context()), r.URL.Query().Get("asset_id"))
	if err != nil {
		log.Printf("balances for %s: %v", UserID(r.Context()), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
