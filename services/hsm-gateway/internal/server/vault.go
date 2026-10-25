package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/gateway"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/vault"
)

// SealedResponse is the 200 body of POST /v1/vault/seal and /reseal: the row to store (base64 in JSON).
type SealedResponse struct {
	Role       string `json:"role"`
	Purpose    string `json:"purpose"`
	Version    int    `json:"version"`
	KEKVersion int    `json:"kek_version"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	AAD        []byte `json:"aad"`
}

func sealedResponse(r *gateway.SealedRow) SealedResponse {
	return SealedResponse{Role: r.Role, Purpose: r.Purpose, Version: r.Version, KEKVersion: r.KEKVersion, WrappedDEK: r.WrappedDEK,
		Nonce: r.Nonce, Ciphertext: r.Ciphertext, AAD: r.AAD}
}

// vaultError maps vault errors to status codes. The tamper class is one explicit answer: 422 authentication_failed.
func vaultError(w http.ResponseWriter, err error) {
	var vr *gateway.VaultRefused
	var he *hsm.Error
	switch {
	case errors.Is(err, gateway.ErrAuthFailed):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "authentication_failed"})
	case errors.Is(err, hsm.ErrBadNonce):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "bad_nonce"})
	case errors.Is(err, hsm.ErrNoKEK):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "kek_missing"})
	case errors.Is(err, gateway.ErrVaultUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_unavailable"})
	case errors.Is(err, gateway.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
	case errors.Is(err, gateway.ErrWindowClosed):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintenance_window_closed"})
	case errors.As(err, &vr):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "vault_refused", "rule": vr.Rule})
	case errors.Is(err, gateway.ErrAudit):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit_unavailable"})
	case errors.Is(err, vault.ErrShape):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "detail": err.Error()})
	case errors.As(err, &he):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": he.Code})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": hsm.CodeUnavailable})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func (s *Server) vaultSeal(w http.ResponseWriter, r *http.Request) {
	q, err := vault.DecodeSeal(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		vaultError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	row, err := s.svc.Seal(ctx, authz.CallerCN(r.Context()), q)
	if err != nil {
		vaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sealedResponse(row))
}

// vaultUnseal answers with the raw plaintext as application/octet-stream and nothing else: the caller reads it straight into its locked
// page. The buffer is zeroed after the write.
func (s *Server) vaultUnseal(w http.ResponseWriter, r *http.Request) {
	row, err := vault.DecodeRow(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		vaultError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	pt, err := s.svc.Unseal(ctx, authz.CallerCN(r.Context()), row)
	if err != nil {
		vaultError(w, err)
		return
	}
	defer clear(pt)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(pt)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pt)
}

func (s *Server) vaultVerify(w http.ResponseWriter, r *http.Request) {
	row, err := vault.DecodeRow(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		vaultError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	x, err := s.svc.VerifyXPub(ctx, authz.CallerCN(r.Context()), row)
	if err != nil {
		vaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"xpub": x, "path": "m/44'/60'/0'"})
}

func (s *Server) vaultReseal(w http.ResponseWriter, r *http.Request) {
	q, err := vault.DecodeReseal(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		vaultError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	row, err := s.svc.Reseal(ctx, authz.CallerCN(r.Context()), q)
	if err != nil {
		vaultError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sealedResponse(row))
}

func (s *Server) vaultKEKs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), SignDeadline)
	defer cancel()
	ks, err := s.svc.KEKs(ctx)
	if err != nil {
		vaultError(w, err)
		return
	}
	b, _ := json.Marshal(map[string]any{"keks": ks})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

func (s *Server) kekAdmin(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := vault.DecodeKEK(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			vaultError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*SignDeadline) // key generation on a real HSM can be slow
		defer cancel()
		created, err := s.svc.KEKAdmin(ctx, authz.CallerCN(r.Context()), kind, q.Version)
		if err != nil {
			vaultError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": q.Version, "created": created})
	})
}
