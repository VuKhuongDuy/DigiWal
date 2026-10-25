// Package gateway is the sign path (key-custody-hsm.md 5.3): rebuild the digest from the intent,
// pass the Signing Policy, append + fsync the audit entry, ask the HSM, normalise to low-s and find
// v by recovering the pinned Hot address, and only then return the signed transaction.
package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/prometheus/client_golang/prometheus"

	pkgmetrics "digiwal/pkg/metrics"
	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/intent"
	"digiwal/services/hsm-gateway/internal/policy"
	"digiwal/services/hsm-gateway/internal/sigutil"
)

// Sentinel errors the HTTP layer maps to status codes.
var (
	// ErrNotServing: the self-test failed (mismatch latched, or a transient HSM failure): /v1/hot/sign is closed.
	ErrNotServing = errors.New("hot signing is not serving: self-test failed")
	// ErrAudit: the audit entry could not be written; no signature is produced.
	ErrAudit = errors.New("audit log write failed")
	// ErrRecovery: the token's signature did not recover the pinned Hot address.
	ErrRecovery = errors.New("signature does not recover the pinned Hot address")
)

// PolicyRefused is a Signing Policy refusal; no signature was produced.
type PolicyRefused struct{ Rule string }

func (e *PolicyRefused) Error() string { return "policy refused: " + e.Rule }

// Result is a successful sign.
type Result struct {
	RawTx      []byte
	TxHash     common.Hash
	From       common.Address
	AuditSeq   uint64
	PolicyHash string
	Rule       string
}

// Service signs Hot transactions.
type Service struct {
	engine *policy.Engine
	log    *audit.Log
	hsm    hsm.Backend
	hot    common.Address

	attestor hsm.Attestor // nil: the token has no attestation key (POST /v1/attest answers 503 attest_unavailable)
	vault    *vaultState  // nil: the token has no KEK (POST /v1/vault/* answers 503 vault_unavailable)

	mismatch atomic.Bool // permanent until restart: wrong key / wrong v / swapped key
	degraded atomic.Bool // cleared by the next passing self-test: HSM errors
	m        *metrics
}

// New builds the service: the HSM key must be exactly the policy-pinned Hot address, and the
// Policy Windows are rebuilt from the audit log.
func New(p *policy.Policy, log_ *audit.Log, replay []audit.Entry, backend hsm.Backend, reg prometheus.Registerer) (*Service, error) {
	if backend.Address() != p.Hot {
		return nil, fmt.Errorf("the HSM key address %s is not the policy-pinned hot_address %s: a changed Hot address is never accepted without a cutover", backend.Address(), p.Hot)
	}
	e := policy.New(p, nil)
	restored := 0
	for _, en := range replay {
		if en.Op != audit.OpHotSign || en.Result != audit.ResultAuthorized {
			continue
		}
		r, err := recordOf(en)
		if err != nil {
			return nil, fmt.Errorf("audit entry %d: %w", en.Seq, err)
		}
		e.Restore(r)
		restored++
	}
	log.Printf("policy windows rebuilt from %d authorized audit entries", restored)
	s := &Service{engine: e, log: log_, hsm: backend, hot: p.Hot, m: newMetrics(reg)}
	return s, nil
}

func recordOf(en audit.Entry) (policy.Record, error) {
	at, err := time.Parse(time.RFC3339Nano, en.TS)
	if err != nil {
		return policy.Record{}, err
	}
	num := func(s string) (*big.Int, error) {
		if s == "" {
			return new(big.Int), nil
		}
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("bad number %q", s)
		}
		return v, nil
	}
	var r policy.Record
	r.At, r.Nonce, r.Class, r.Asset, r.Gas = at, en.Nonce, en.Class, en.Asset, en.Gas
	r.To = common.HexToAddress(en.To)
	if r.Data, err = hex.DecodeString(trim0x(en.Data)); err != nil {
		return r, err
	}
	for _, f := range []struct {
		dst **big.Int
		src string
	}{{&r.Amount, en.Amount}, {&r.FeeWei, en.FeeWei}, {&r.Value, en.Value}, {&r.MaxFee, en.MaxFee}, {&r.Tip, en.Tip}} {
		if *f.dst, err = num(f.src); err != nil {
			return r, err
		}
	}
	return r, nil
}

func trim0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

// PublicKey is the Hot Key's 65-byte uncompressed public key.
func (s *Service) PublicKey() []byte { return s.hsm.PublicKey() }

// Hot returns the pinned Hot address.
func (s *Service) Hot() common.Address { return s.hot }

// WithAttestor enables POST /v1/attest with the token's attestation key (nil or a token without the key disables it).
func (s *Service) WithAttestor(a hsm.Attestor) *Service {
	if a != nil && a.HasAttestKey() {
		s.attestor = a
	}
	return s
}

// Serving reports whether /v1/hot/sign is open.
func (s *Service) Serving() bool { return !s.mismatch.Load() && !s.degraded.Load() }

// Engine exposes the policy engine (tests).
func (s *Service) Engine() *policy.Engine { return s.engine }

func entryOf(in *intent.Intent, op, cn, policyHash string) audit.Entry {
	return audit.Entry{
		Op: op, CallerCN: cn, CallerClaim: in.CallerCNClaim, Idempotency: in.IdempotencyKey,
		ChainID: in.ChainID, Nonce: in.Nonce, To: in.To.Hex(), Value: in.Value.String(),
		Data: "0x" + hex.EncodeToString(in.Data), Gas: in.Gas, MaxFee: in.MaxFee.String(), Tip: in.Tip.String(),
		Class: in.Operation, PolicyHash: policyHash,
	}
}

// Sign runs the whole path for one Sign Intent from the verified caller cn. The signature is only
// returned after the audit entry that authorizes it has been fsynced.
func (s *Service) Sign(ctx context.Context, cn string, in *intent.Intent) (*Result, error) {
	if !s.Serving() {
		s.m.sign.WithLabelValues("not_serving").Inc()
		return nil, ErrNotServing
	}
	digest := in.Digest() // rebuilt here from the intent; there is no way for a caller to supply it
	pol := s.engine.Policy()

	var entry audit.Entry
	d, err := s.engine.Authorize(in, func(d policy.Decision) error {
		entry = entryOf(in, audit.OpHotSign, cn, pol.Hash)
		entry.Class = d.Class
		entry.Asset, entry.Selector, entry.Replacement = d.Asset, d.Selector, d.Replacement
		entry.Recipient = d.Recipient.Hex()
		entry.Amount, entry.FeeWei = d.Amount.String(), d.FeeWei.String()
		entry.Digest = "0x" + hex.EncodeToString(digest[:])
		entry.Rule = d.Rule
		entry.Result = audit.ResultRefused
		if d.Allowed {
			entry.Result = audit.ResultAuthorized
		}
		var err error
		entry, err = s.log.Append(entry)
		return err
	})
	if err != nil {
		s.m.sign.WithLabelValues("audit_error").Inc()
		log.Printf("ALERT audit write failed, no signature produced: %v", err)
		return nil, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	if !d.Allowed {
		s.m.sign.WithLabelValues("refused").Inc()
		s.m.refusals.WithLabelValues(d.Rule).Inc()
		log.Printf("ALERT policy_refused rule=%s caller=%s nonce=%d class=%s policy=%.12s", d.Rule, cn, in.Nonce, in.Operation, pol.Hash)
		return nil, &PolicyRefused{Rule: d.Rule}
	}
	log.Printf("sign authorized seq=%d rule=%s caller=%s nonce=%d class=%s policy=%.12s", entry.Seq, d.Rule, cn, in.Nonce, d.Class, pol.Hash)

	raw, err := s.signAndVerify(ctx, digest[:])
	if err != nil {
		s.failAudit(in, cn, pol.Hash, digest, err)
		return nil, err
	}
	tx, rawTx, err := in.Seal(raw)
	if err != nil {
		s.failAudit(in, cn, pol.Hash, digest, err)
		return nil, err
	}
	// Independent check on the assembled transaction, the way wallet-service will do it.
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil || from != s.hot {
		s.mismatch.Store(true)
		s.failAudit(in, cn, pol.Hash, digest, ErrRecovery)
		return nil, ErrRecovery
	}
	s.m.sign.WithLabelValues("signed").Inc()
	return &Result{RawTx: rawTx, TxHash: tx.Hash(), From: from, AuditSeq: entry.Seq, PolicyHash: pol.Hash, Rule: d.Rule}, nil
}

// signAndVerify asks the token and returns the canonical 65-byte signature. A signature that does
// not recover the pinned address is never returned and latches the gateway closed.
func (s *Service) signAndVerify(ctx context.Context, digest []byte) ([]byte, error) {
	rs, err := s.hsm.SignDigest(ctx, digest)
	if err != nil {
		s.m.hsmErrors.WithLabelValues(hsmCode(err)).Inc()
		return nil, err
	}
	sig, err := sigutil.Finalize(digest, rs, s.hot)
	if err != nil {
		s.mismatch.Store(true)
		s.m.hsmErrors.WithLabelValues("recovery_mismatch").Inc()
		log.Printf("ALERT hsm signature does not recover the pinned Hot address: closing /v1/hot/sign (%v)", err)
		return nil, fmt.Errorf("%w: %v", ErrRecovery, err)
	}
	return sig, nil
}

// failAudit records that an authorized request produced no signature (best effort: the authorizing
// entry is already durable, and the Policy Window counts the request regardless).
func (s *Service) failAudit(in *intent.Intent, cn, policyHash string, digest [32]byte, cause error) {
	e := entryOf(in, audit.OpHotSign, cn, policyHash)
	e.Digest = "0x" + hex.EncodeToString(digest[:])
	e.Result, e.Rule, e.Detail = audit.ResultHSMError, "hsm_error", cause.Error()
	if _, err := s.log.Append(e); err != nil {
		log.Printf("audit: could not record hsm_error: %v", err)
	}
	s.m.sign.WithLabelValues("hsm_error").Inc()
}

func hsmCode(err error) string {
	var he *hsm.Error
	if errors.As(err, &he) {
		return he.Code
	}
	return "other"
}

// SelfTest checks the key attributes, signs the fixed never-mineable transaction (nonce 2^64-1)
// and verifies recovery equals the pinned Hot address, in canonical low-s form. The attempt is
// audited. A mismatch latches the gateway closed; an HSM error closes it until the next pass.
func (s *Service) SelfTest(ctx context.Context) error {
	err := s.selfTest(ctx)
	switch {
	case err == nil:
		s.degraded.Store(false)
		s.m.selfTestOK.Set(1)
	case errors.Is(err, ErrRecovery):
		s.mismatch.Store(true)
		s.m.selfTestOK.Set(0)
		log.Printf("ALERT self-test mismatch, /v1/hot/sign closed until restart: %v", err)
	default:
		s.degraded.Store(true)
		s.m.selfTestOK.Set(0)
		log.Printf("ALERT self-test failed, /v1/hot/sign closed until it passes: %v", err)
	}
	return err
}

func (s *Service) selfTest(ctx context.Context) error {
	if err := s.hsm.CheckKey(ctx); err != nil {
		return err
	}
	in := intent.SelfTestIntent(s.engine.Policy().ChainID, s.hot)
	digest := in.Digest()
	e := entryOf(in, audit.OpSelfTest, "self", s.engine.Policy().Hash)
	e.Digest, e.Rule, e.Result = "0x"+hex.EncodeToString(digest[:]), "self_test", audit.ResultAuthorized
	if _, err := s.log.Append(e); err != nil {
		return fmt.Errorf("%w: %v", ErrAudit, err)
	}
	rs, err := s.hsm.SignDigest(ctx, digest[:])
	if err != nil {
		return err
	}
	sig, err := sigutil.Finalize(digest[:], rs, s.hot)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRecovery, err)
	}
	if !sigutil.IsLowS(sig) {
		return fmt.Errorf("%w: high s", ErrRecovery)
	}
	tx, _, err := in.Seal(sig)
	if err != nil {
		return err
	}
	if from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx); err != nil || from != s.hot {
		return ErrRecovery
	}
	return nil
}

// RunSelfTests runs SelfTest every interval until ctx ends, calling after (if set) with each result.
func (s *Service) RunSelfTests(ctx context.Context, interval time.Duration, after func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.SelfTest(c)
			cancel()
			if after != nil {
				after(err)
			}
		}
	}
}

// AuditHead returns the log head.
func (s *Service) AuditHead() (uint64, string) { return s.log.Head() }

// Ready: the key passed its last self-test and the audit log is writable.
func (s *Service) Ready() bool { return s.Serving() && s.log.Healthy() }

type metrics struct {
	sign       *prometheus.CounterVec
	attests    *prometheus.CounterVec
	refusals   *prometheus.CounterVec
	hsmErrors  *prometheus.CounterVec
	selfTestOK prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	if err := pkgmetrics.CheckLabels("result", "rule", "reason"); err != nil {
		panic(err)
	}
	m := &metrics{
		sign:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_hsm_gateway_sign_total", Help: "Hot sign requests by result."}, []string{"result"}),
		attests:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_hsm_gateway_attest_total", Help: "xpub attestation requests by result."}, []string{"result"}),
		refusals:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_hsm_gateway_policy_refusals_total", Help: "Signing Policy refusals by rule id (alert on any)."}, []string{"rule"}),
		hsmErrors:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_hsm_gateway_hsm_errors_total", Help: "HSM failures by mapped error class."}, []string{"reason"}),
		selfTestOK: prometheus.NewGauge(prometheus.GaugeOpts{Name: "digiwal_hsm_gateway_self_test_ok", Help: "1 if the last self-test (sign + recover) passed."}),
	}
	if reg != nil {
		reg.MustRegister(m.sign, m.attests, m.refusals, m.hsmErrors, m.selfTestOK)
	}
	return m
}
