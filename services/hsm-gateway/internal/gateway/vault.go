package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/prometheus/client_golang/prometheus"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/vault"
)

// Vault errors the HTTP layer maps to status codes.
var (
	// ErrVaultUnavailable: the token has no KEK (created before M3c; run `init-kek`).
	ErrVaultUnavailable = errors.New("the token has no KEK: vault operations are unavailable")
	// ErrRateLimited: more unseals in the last minute than the cap.
	ErrRateLimited = errors.New("unseal rate limit reached")
	// ErrWindowClosed: a KEK administration call outside a Maintenance Window.
	ErrWindowClosed = errors.New("no Maintenance Window is open")
	// ErrAuthFailed is the explicit "authentication failed": AAD mismatch, tamper, wrong KEK.
	ErrAuthFailed = hsm.ErrAuthFailed
)

// VaultRefused is a refusal by rule; nothing was asked of the token.
type VaultRefused struct{ Rule string }

func (e *VaultRefused) Error() string { return "vault refused: " + e.Rule }

// VaultOptions configures the vault half of the service.
type VaultOptions struct {
	// Specs is the (role -> purpose, size, unseal callers) table; nil means vault.DefaultSpecs().
	Specs map[string]vault.Spec
	// UnsealPerMinute caps POST /v1/vault/unseal (invariant 17, default 60).
	UnsealPerMinute int
	// Window reports whether a Maintenance Window is open; nil means never (every admin call is refused).
	Window func(now time.Time) (open bool, until time.Time)
	// Now is the clock (tests).
	Now func() time.Time
	// Registry receives the vault metrics (nil: not registered).
	Registry prometheus.Registerer
}

type vaultState struct {
	v      hsm.Vault
	specs  map[string]vault.Spec
	window func(time.Time) (bool, time.Time)
	now    func() time.Time
	ops    *prometheus.CounterVec

	mu       sync.Mutex
	limit    int
	unsealed []time.Time // timestamps of unseals inside the last minute
}

// WithVault enables the vault operations with the token's KEK capability. A token without a KEK leaves them answering 503.
func (s *Service) WithVault(v hsm.Vault, o VaultOptions) *Service {
	if v == nil || !v.HasKEK() {
		return s
	}
	st := &vaultState{v: v, specs: o.Specs, window: o.Window, now: o.Now, limit: o.UnsealPerMinute}
	if st.specs == nil {
		st.specs = vault.DefaultSpecs()
	}
	if st.now == nil {
		st.now = time.Now
	}
	if st.limit <= 0 {
		st.limit = 60
	}
	st.ops = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_hsm_gateway_vault_total", Help: "Vault operations by operation and outcome (alert on any authentication_failed or caller_not_allowed)."}, []string{"result"})
	if o.Registry != nil {
		o.Registry.MustRegister(st.ops)
	}
	s.vault = st
	return s
}

// VaultEnabled reports whether the vault operations are available.
func (s *Service) VaultEnabled() bool { return s.vault != nil }

func vaultEntry(op, cn, result, rule, detail, digest, policyHash string) audit.Entry {
	return audit.Entry{
		Op: op, CallerCN: cn, To: "", Value: "0", Data: "0x", MaxFee: "0", Tip: "0",
		Class: "vault", Digest: digest, Rule: rule, Result: result, Detail: detail, PolicyHash: policyHash,
	}
}

func rowDetail(role, purpose string, version, kek int, wrapped []byte) string {
	return fmt.Sprintf("role=%s purpose=%s version=%d kek=%d wrapped_dek=%s", role, purpose, version, kek, vault.Fingerprint(wrapped))
}

// refuse audits a refusal by rule and returns the typed error. A failed audit write wins: it is reported instead (fail closed).
func (st *vaultState) refuse(s *Service, op, cn, rule, detail string, cause error) error {
	pol := s.engine.Policy().Hash
	if _, err := s.log.Append(vaultEntry(op, cn, audit.ResultRefused, rule, detail, "", pol)); err != nil {
		st.ops.WithLabelValues(op + "_audit_error").Inc()
		log.Printf("ALERT audit write failed, vault refusal not recorded: %v", err)
		return fmt.Errorf("%w: %v", ErrAudit, err)
	}
	st.ops.WithLabelValues(op + "_" + rule).Inc()
	log.Printf("ALERT vault %s refused rule=%s caller=%s %s", op, rule, cn, detail)
	if cause != nil {
		return cause
	}
	return &VaultRefused{Rule: rule}
}

// authorize appends the "authorized" line (fsynced) that must exist before the token is asked.
func (st *vaultState) authorize(s *Service, op, cn, rule, detail string) error {
	if _, err := s.log.Append(vaultEntry(op, cn, audit.ResultAuthorized, rule, detail, "", s.engine.Policy().Hash)); err != nil {
		st.ops.WithLabelValues(op + "_audit_error").Inc()
		log.Printf("ALERT audit write failed, no vault operation performed: %v", err)
		return fmt.Errorf("%w: %v", ErrAudit, err)
	}
	return nil
}

// failed records that an authorized operation produced nothing (best effort: the authorizing line is already durable).
func (st *vaultState) failed(s *Service, op, cn, rule, detail string, cause error) {
	e := vaultEntry(op, cn, audit.ResultHSMError, rule, detail+" error="+cause.Error(), "", s.engine.Policy().Hash)
	if _, err := s.log.Append(e); err != nil {
		log.Printf("audit: could not record the vault failure: %v", err)
	}
	st.ops.WithLabelValues(op + "_" + rule).Inc()
	if errors.Is(cause, ErrAuthFailed) {
		log.Printf("ALERT vault %s authentication failed caller=%s %s", op, cn, detail)
	}
}

func (st *vaultState) allowUnseal() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	cut := now.Add(-time.Minute)
	keep := st.unsealed[:0]
	for _, t := range st.unsealed {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	st.unsealed = keep
	if len(st.unsealed) >= st.limit {
		return false
	}
	st.unsealed = append(st.unsealed, now)
	return true
}

func (st *vaultState) needWindow(s *Service, op, cn, detail string) (until time.Time, err error) {
	if st.window != nil {
		if open, u := st.window(st.now()); open {
			return u, nil
		}
	}
	return time.Time{}, st.refuse(s, op, cn, "maintenance_window_closed", detail, ErrWindowClosed)
}

// specFor checks the role/purpose pair against the table (and, for unseal, the caller CN against the role).
func (st *vaultState) specFor(s *Service, op, cn, role, purpose string, unseal bool, detail string) (vault.Spec, error) {
	spec, ok := st.specs[role]
	switch {
	case !ok:
		return spec, st.refuse(s, op, cn, "role_unknown", detail, nil)
	case spec.Purpose != purpose:
		return spec, st.refuse(s, op, cn, "purpose_mismatch", detail, nil)
	case unseal && !slices.Contains(spec.Unseal, cn):
		return spec, st.refuse(s, op, cn, "caller_not_allowed", detail, nil)
	}
	return spec, nil
}

// checkRow validates the row's shape, rebuilds the AAD from its fields and compares it with the stored one. A mismatch is the
// explicit "authentication failed" (a row moved between roles, versions, purposes, KEKs or wrapped DEKs), audited as a refusal.
func (st *vaultState) checkRow(s *Service, op, cn string, r *vault.Row, detail string) ([]byte, error) {
	if r.Version < 1 || r.KEKVersion < 1 || len(r.WrappedDEK) == 0 || len(r.Ciphertext) == 0 {
		return nil, st.refuse(s, op, cn, "bad_row", detail, nil)
	}
	if err := hsm.CheckNonce(r.Nonce); err != nil {
		return nil, st.refuse(s, op, cn, "bad_nonce", detail, err)
	}
	want := vault.AAD(r.Role, r.Purpose, r.Version, r.KEKVersion, r.WrappedDEK)
	if !vault.SameAAD(want, r.AAD) {
		return nil, st.refuse(s, op, cn, "authentication_failed", detail+" (aad does not match role|purpose|version|kek|wrapped_dek)", ErrAuthFailed)
	}
	return want, nil
}

func opened(r *vault.Row) hsm.Opened {
	return hsm.Opened{KEKVersion: r.KEKVersion, WrappedDEK: r.WrappedDEK, Nonce: r.Nonce, Ciphertext: r.Ciphertext, AAD: r.AAD}
}

// SealedRow is the answer of Seal and Reseal: everything the caller stores.
type SealedRow struct {
	Role, Purpose string
	Version       int
	KEKVersion    int
	WrappedDEK    []byte
	Nonce         []byte
	Ciphertext    []byte
	AAD           []byte
}

// newestWrappingKEK is the highest KEK version that can still wrap.
func (st *vaultState) newestWrappingKEK(ctx context.Context) (int, error) {
	ks, err := st.v.KEKs(ctx)
	if err != nil {
		return 0, err
	}
	best := 0
	for _, k := range ks {
		if k.Wrap && k.Version > best {
			best = k.Version
		}
	}
	if best == 0 {
		return 0, hsm.ErrNoKEK
	}
	return best, nil
}

// Seal generates a DEK inside the token, wraps it with the KEK, encrypts the plaintext inside the token and returns the row. The
// plaintext never leaves this process and the DEK never leaves the token.
func (s *Service) Seal(ctx context.Context, cn string, q *vault.SealRequest) (*SealedRow, error) {
	st := s.vault
	if st == nil {
		return nil, ErrVaultUnavailable
	}
	detail := fmt.Sprintf("role=%s purpose=%s version=%d", q.Role, q.Purpose, q.Version)
	spec, err := st.specFor(s, "seal", cn, q.Role, q.Purpose, false, detail)
	if err != nil {
		return nil, err
	}
	if q.Version < 1 || len(q.Plaintext) != spec.PlainLen {
		clear(q.Plaintext)
		return nil, st.refuse(s, "seal", cn, "bad_request", detail, nil)
	}
	defer clear(q.Plaintext)
	kek := q.KEKVersion
	if kek == 0 {
		if kek, err = st.newestWrappingKEK(ctx); err != nil {
			return nil, err
		}
	}
	detail += fmt.Sprintf(" kek=%d", kek)
	if err := st.authorize(s, "seal", cn, "seal", detail); err != nil {
		return nil, err
	}
	out, err := st.v.Seal(ctx, kek, q.Plaintext, func(w []byte) []byte { return vault.AAD(q.Role, q.Purpose, q.Version, kek, w) })
	if err != nil {
		st.failed(s, "seal", cn, "hsm_error", detail, err)
		return nil, err
	}
	st.ops.WithLabelValues("seal_ok").Inc()
	log.Printf("vault seal ok caller=%s %s", cn, detail)
	return &SealedRow{Role: q.Role, Purpose: q.Purpose, Version: q.Version, KEKVersion: out.KEKVersion, WrappedDEK: out.WrappedDEK,
		Nonce: out.Nonce, Ciphertext: out.Ciphertext, AAD: out.AAD}, nil
}

// Unseal returns the plaintext of a row to its allowed caller: role and purpose in the table, the caller CN allowed for the role, the
// rate cap, the AAD rebuilt from the row's fields, and the audit line fsynced BEFORE the token is asked. The caller must zero the result.
func (s *Service) Unseal(ctx context.Context, cn string, r *vault.Row) ([]byte, error) {
	st := s.vault
	if st == nil {
		return nil, ErrVaultUnavailable
	}
	detail := rowDetail(r.Role, r.Purpose, r.Version, r.KEKVersion, r.WrappedDEK)
	spec, err := st.specFor(s, "unseal", cn, r.Role, r.Purpose, true, detail)
	if err != nil {
		return nil, err
	}
	if !st.allowUnseal() {
		return nil, st.refuse(s, "unseal", cn, "rate_limited", detail, ErrRateLimited)
	}
	aad, err := st.checkRow(s, "unseal", cn, r, detail)
	if err != nil {
		return nil, err
	}
	if err := st.authorize(s, "unseal", cn, "unseal", detail); err != nil {
		return nil, err
	}
	pt, err := st.v.Unseal(ctx, opened(r), aad)
	if err != nil {
		rule := "hsm_error"
		if errors.Is(err, ErrAuthFailed) {
			rule = "authentication_failed"
		}
		st.failed(s, "unseal", cn, rule, detail, err)
		return nil, err
	}
	if len(pt) != spec.PlainLen {
		clear(pt)
		err := fmt.Errorf("%w: the plaintext is not %d bytes", ErrAuthFailed, spec.PlainLen)
		st.failed(s, "unseal", cn, "authentication_failed", detail, err)
		return nil, err
	}
	st.ops.WithLabelValues("unseal_ok").Inc()
	return pt, nil
}

// VerifyXPub is for key-admin: it opens a row inside the gateway and returns only the PUBLIC account xpub (m/44'/60'/0') of the seed it
// holds, so a rotation can prove "this row holds the seed the attested xpub belongs to" without the seed ever reaching key-admin. It is
// not rate limited (key-admin only) and audited like an unseal.
func (s *Service) VerifyXPub(ctx context.Context, cn string, r *vault.Row) (string, error) {
	st := s.vault
	if st == nil {
		return "", ErrVaultUnavailable
	}
	detail := rowDetail(r.Role, r.Purpose, r.Version, r.KEKVersion, r.WrappedDEK)
	if r.Role != vault.RoleUserDerivation {
		return "", st.refuse(s, "verify", cn, "role_unknown", detail, nil)
	}
	if _, err := st.specFor(s, "verify", cn, r.Role, r.Purpose, false, detail); err != nil {
		return "", err
	}
	aad, err := st.checkRow(s, "verify", cn, r, detail)
	if err != nil {
		return "", err
	}
	if err := st.authorize(s, "verify", cn, "verify", detail); err != nil {
		return "", err
	}
	pt, err := st.v.Unseal(ctx, opened(r), aad)
	if err != nil {
		rule := "hsm_error"
		if errors.Is(err, ErrAuthFailed) {
			rule = "authentication_failed"
		}
		st.failed(s, "verify", cn, rule, detail, err)
		return "", err
	}
	defer clear(pt)
	x, err := accountXPub(pt)
	if err != nil {
		st.failed(s, "verify", cn, "bad_seed", detail, err)
		return "", err
	}
	st.ops.WithLabelValues("verify_ok").Inc()
	return x, nil
}

// accountXPub derives the public account node m/44'/60'/0' from a 64-byte BIP32 seed and zeroes every private node it made.
func accountXPub(seed []byte) (string, error) {
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return "", err
	}
	nodes := []*hdkeychain.ExtendedKey{k}
	defer func() {
		for _, n := range nodes {
			n.Zero()
		}
	}()
	for _, i := range []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart} {
		if k, err = k.Derive(i); err != nil {
			return "", err
		}
		nodes = append(nodes, k)
	}
	pub, err := k.Neuter()
	if err != nil {
		return "", err
	}
	return pub.String(), nil
}

// Reseal re-encrypts a row inside the token under a new row version and KEK (rewrap, KEK rotation) and optionally a new DEK (DEK rotation).
// Needs a Maintenance Window. The caller never sees the plaintext.
func (s *Service) Reseal(ctx context.Context, cn string, q *vault.ResealRequest) (*SealedRow, error) {
	st := s.vault
	if st == nil {
		return nil, ErrVaultUnavailable
	}
	r := &q.Row
	detail := rowDetail(r.Role, r.Purpose, r.Version, r.KEKVersion, r.WrappedDEK) + fmt.Sprintf(" to_version=%d new_dek=%v", q.ToVersion, q.NewDEK)
	if _, err := st.specFor(s, "reseal", cn, r.Role, r.Purpose, false, detail); err != nil {
		return nil, err
	}
	until, err := st.needWindow(s, "reseal", cn, detail)
	if err != nil {
		return nil, err
	}
	if q.ToVersion < 1 || q.ToVersion == r.Version {
		return nil, st.refuse(s, "reseal", cn, "bad_request", detail+" (to_version must differ from the row's version)", nil)
	}
	aad, err := st.checkRow(s, "reseal", cn, r, detail)
	if err != nil {
		return nil, err
	}
	to := q.ToKEK
	if to == 0 {
		if to, err = st.newestWrappingKEK(ctx); err != nil {
			return nil, err
		}
	}
	detail += fmt.Sprintf(" to_kek=%d window_until=%s", to, until.UTC().Format(time.RFC3339))
	if err := st.authorize(s, "reseal", cn, "reseal", detail); err != nil {
		return nil, err
	}
	out, err := st.v.Reseal(ctx, opened(r), aad, to, q.NewDEK, func(w []byte) []byte { return vault.AAD(r.Role, r.Purpose, q.ToVersion, to, w) })
	if err != nil {
		rule := "hsm_error"
		if errors.Is(err, ErrAuthFailed) {
			rule = "authentication_failed"
		}
		st.failed(s, "reseal", cn, rule, detail, err)
		return nil, err
	}
	st.ops.WithLabelValues("reseal_ok").Inc()
	log.Printf("vault reseal ok caller=%s %s", cn, detail)
	return &SealedRow{Role: r.Role, Purpose: r.Purpose, Version: q.ToVersion, KEKVersion: out.KEKVersion, WrappedDEK: out.WrappedDEK,
		Nonce: out.Nonce, Ciphertext: out.Ciphertext, AAD: out.AAD}, nil
}

// KEKs lists the KEK versions (read only, not audited).
func (s *Service) KEKs(ctx context.Context) ([]hsm.KEKInfo, error) {
	if s.vault == nil {
		return nil, ErrVaultUnavailable
	}
	return s.vault.v.KEKs(ctx)
}

// KEKAdmin runs one KEK administration call (generate, retire, destroy) inside a Maintenance Window. created is only meaningful for generate.
func (s *Service) KEKAdmin(ctx context.Context, cn, kind string, version int) (created bool, err error) {
	st := s.vault
	if st == nil {
		return false, ErrVaultUnavailable
	}
	op := "kek_" + kind
	detail := fmt.Sprintf("kek=%d", version)
	if kind != "generate" && kind != "retire" && kind != "destroy" {
		return false, st.refuse(s, op, cn, "bad_request", detail, nil)
	}
	until, err := st.needWindow(s, op, cn, detail)
	if err != nil {
		return false, err
	}
	detail += " window_until=" + until.UTC().Format(time.RFC3339)
	if err := st.authorize(s, op, cn, op, detail); err != nil {
		return false, err
	}
	switch kind {
	case "generate":
		created, err = st.v.GenerateKEK(ctx, version)
	case "retire":
		err = st.v.RetireKEK(ctx, version)
	case "destroy":
		err = st.v.DestroyKEK(ctx, version)
	}
	if err != nil {
		st.failed(s, op, cn, "hsm_error", detail, err)
		return false, err
	}
	st.ops.WithLabelValues(op + "_ok").Inc()
	log.Printf("vault %s ok caller=%s %s created=%v", op, cn, detail, created)
	return created, nil
}
