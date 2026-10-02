package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Withdrawals is the slice of withdrawal-service the gateway calls. Every method takes the
// authenticated user_id from the gateway, never from the client, and returns withdrawal-service's
// answer verbatim (integration-contracts.md §2/§3).
type Withdrawals interface {
	Do(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error)
}

// WithdrawalClient calls withdrawal-service over mTLS as CN=api-gateway (Decided by the Tech Lead,
// service-auth-mtls.md Open Questions #4 for this hop).
type WithdrawalClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *WithdrawalClient) Do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("reading withdrawal-service response: %w", err)
	}
	return resp.StatusCode, out, nil
}

// decodeClient reads a client body strictly: an unknown field — notably a client-supplied
// user_id — is a 400, never silently passed on (identity-auth.md invariant #4).
func decodeClient(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body (user_id may not be supplied)"})
		return false
	}
	return true
}

// proxy calls withdrawal-service and relays its answer. A transport failure is a 502.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, method, path string, body any) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, out, err := s.Withdrawals.Do(ctx, method, path, body)
	if err != nil {
		logUpstream("withdrawal-service", method, path, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func userQuery(r *http.Request) string { return "user_id=" + url.QueryEscape(UserID(r.Context())) }

// POST /v1/withdrawals {idempotency_key, asset_id, amount, to_address}.
func (s *Server) handleCreateWithdrawal(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IdempotencyKey string `json:"idempotency_key"`
		AssetID        string `json:"asset_id"`
		Amount         string `json:"amount"`
		ToAddress      string `json:"to_address"`
	}
	if !decodeClient(w, r, &b) {
		return
	}
	s.proxy(w, r, http.MethodPost, "/v1/withdrawals", map[string]string{
		"user_id": UserID(r.Context()), "idempotency_key": b.IdempotencyKey,
		"asset_id": b.AssetID, "amount": b.Amount, "to_address": b.ToAddress,
	})
}

// GET /v1/withdrawals/{idempotency_key}: the caller's own request only (withdrawal-service
// answers 404 for another user's key, invariant #20).
func (s *Server) handleGetWithdrawal(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, http.MethodGet, "/v1/withdrawals/"+url.PathEscape(r.PathValue("idempotency_key"))+"?"+userQuery(r), nil)
}

// GET /v1/whitelist: the caller's own entries.
func (s *Server) handleListWhitelist(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, http.MethodGet, "/v1/whitelist?"+userQuery(r), nil)
}

// POST /v1/whitelist {address}.
func (s *Server) handleAddWhitelist(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Address string `json:"address"`
	}
	if !decodeClient(w, r, &b) {
		return
	}
	s.proxy(w, r, http.MethodPost, "/v1/whitelist", map[string]string{"user_id": UserID(r.Context()), "address": b.Address})
}

// DELETE /v1/whitelist/{id}: the caller's own entry; another user's id is a 204 no-op.
func (s *Server) handleRemoveWhitelist(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, http.MethodDelete, "/v1/whitelist/"+url.PathEscape(r.PathValue("id"))+"?"+userQuery(r), nil)
}
