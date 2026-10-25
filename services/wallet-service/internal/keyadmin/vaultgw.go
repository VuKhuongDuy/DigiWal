package keyadmin

import (
	"context"
	"encoding/json"
	"net/http"

	"digiwal/services/wallet-service/internal/vaultstore"
)

// Sealed is a Sealed Secret as hsm-gateway returns and accepts it (base64 in JSON).
type Sealed struct {
	Role       string `json:"role"`
	Purpose    string `json:"purpose"`
	Version    int    `json:"version"`
	KEKVersion int    `json:"kek_version"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	AAD        []byte `json:"aad"`
}

// SealedOf is the wire form of a stored row.
func SealedOf(r vaultstore.Row) Sealed {
	return Sealed{Role: r.Role, Purpose: r.Purpose, Version: r.Version, KEKVersion: r.KEKVersion, WrappedDEK: r.WrappedDEK, Nonce: r.Nonce, Ciphertext: r.Ciphertext, AAD: r.AAD}
}

// RowOf is the storable form of a gateway answer (pending).
func RowOf(s Sealed) vaultstore.Row {
	return vaultstore.Row{Role: s.Role, Version: s.Version, Status: vaultstore.Pending, Purpose: s.Purpose, KEKVersion: s.KEKVersion,
		WrappedDEK: s.WrappedDEK, Nonce: s.Nonce, Ciphertext: s.Ciphertext, AAD: s.AAD}
}

// KEK is one KEK version in the token.
type KEK struct {
	Version int  `json:"version"`
	Wrap    bool `json:"wrap"`
}

// VaultGateway is what the vault ceremonies need from hsm-gateway. key-admin never receives a plaintext from it except the account
// xpub, which is public.
type VaultGateway interface {
	Seal(ctx context.Context, role, purpose string, version, kek int, plaintext []byte) (Sealed, error)
	// VerifyXPub opens the row inside the gateway and returns only the account xpub (m/44'/60'/0') of the seed it holds.
	VerifyXPub(ctx context.Context, row Sealed) (string, error)
	// Reseal re-encrypts inside the token under toVersion and toKEK (0 = newest wrapping); newDEK also replaces the DEK.
	Reseal(ctx context.Context, row Sealed, toVersion, toKEK int, newDEK bool) (Sealed, error)
	KEKs(ctx context.Context) ([]KEK, error)
	GenerateKEK(ctx context.Context, version int) (created bool, err error)
	RetireKEK(ctx context.Context, version int) error
	DestroyKEK(ctx context.Context, version int) error
}

var _ VaultGateway = (*GatewayClient)(nil)

// Seal implements VaultGateway (POST /v1/vault/seal).
func (c *GatewayClient) Seal(ctx context.Context, role, purpose string, version, kek int, plaintext []byte) (Sealed, error) {
	body, _ := json.Marshal(map[string]any{"role": role, "purpose": purpose, "version": version, "kek_version": kek, "plaintext": plaintext})
	var out Sealed
	_, err := c.do(ctx, http.MethodPost, "/v1/vault/seal", body, &out)
	return out, err
}

// VerifyXPub implements VaultGateway (POST /v1/vault/verify).
func (c *GatewayClient) VerifyXPub(ctx context.Context, row Sealed) (string, error) {
	body, _ := json.Marshal(row)
	var out struct {
		XPub string `json:"xpub"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/vault/verify", body, &out)
	return out.XPub, err
}

// Reseal implements VaultGateway (POST /v1/vault/reseal).
func (c *GatewayClient) Reseal(ctx context.Context, row Sealed, toVersion, toKEK int, newDEK bool) (Sealed, error) {
	body, _ := json.Marshal(map[string]any{"row": row, "to_version": toVersion, "to_kek_version": toKEK, "new_dek": newDEK})
	var out Sealed
	_, err := c.do(ctx, http.MethodPost, "/v1/vault/reseal", body, &out)
	return out, err
}

// KEKs implements VaultGateway (GET /v1/vault/keks).
func (c *GatewayClient) KEKs(ctx context.Context) ([]KEK, error) {
	var out struct {
		KEKs []KEK `json:"keks"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/vault/keks", nil, &out)
	return out.KEKs, err
}

func (c *GatewayClient) kekCall(ctx context.Context, action string, version int) (bool, error) {
	body, _ := json.Marshal(map[string]int{"version": version})
	var out struct {
		Created bool `json:"created"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/vault/kek/"+action, body, &out)
	return out.Created, err
}

// GenerateKEK implements VaultGateway (POST /v1/vault/kek/generate; needs an open Maintenance Window).
func (c *GatewayClient) GenerateKEK(ctx context.Context, version int) (bool, error) {
	return c.kekCall(ctx, "generate", version)
}

// RetireKEK implements VaultGateway (POST /v1/vault/kek/retire).
func (c *GatewayClient) RetireKEK(ctx context.Context, version int) error {
	_, err := c.kekCall(ctx, "retire", version)
	return err
}

// DestroyKEK implements VaultGateway (POST /v1/vault/kek/destroy).
func (c *GatewayClient) DestroyKEK(ctx context.Context, version int) error {
	_, err := c.kekCall(ctx, "destroy", version)
	return err
}
