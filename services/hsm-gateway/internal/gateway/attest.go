package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	"digiwal/services/hsm-gateway/internal/attest"
	"digiwal/services/hsm-gateway/internal/audit"
)

// Attest errors the HTTP layer maps to status codes.
var (
	// ErrAttestUnavailable: the token has no attestation key (initialised before M3a; run `init-attest-key`).
	ErrAttestUnavailable = errors.New("the token has no attestation key")
	// ErrAttestShape: the request is not exactly the allowed (chain, path, xpub) shape. Nothing was signed.
	ErrAttestShape = attest.ErrShape
)

// AttestResult is a successful attestation.
type AttestResult struct {
	Statement string
	Signature []byte // raw 64-byte r||s over SHA-256(Statement), P-256
	PublicKey []byte // the attestation public key, 65 bytes
	AuditSeq  uint64
}

func attestEntry(cn string, outcome, rule, detail, digest string, policyHash string) audit.Entry {
	return audit.Entry{
		Op: audit.OpAttest, CallerCN: cn, To: "", Value: "0", Data: "0x", MaxFee: "0", Tip: "0",
		Class: "xpub_attest", Digest: digest, Rule: rule, Result: outcome, Detail: detail, PolicyHash: policyHash,
	}
}

// Attest validates the request shape, appends the audit entry (fsynced) and only then asks the token to sign
// the canonical statement. A refusal is audited too and never reaches the token. shapeErr is a decode error from the request body (q is then nil).
func (s *Service) Attest(ctx context.Context, cn string, q *attest.Request, shapeErr error) (*AttestResult, error) {
	pol := s.engine.Policy().Hash
	if s.attestor == nil {
		s.m.attests.WithLabelValues("unavailable").Inc()
		return nil, ErrAttestUnavailable
	}
	if shapeErr == nil && q != nil {
		shapeErr = q.Validate()
	}
	if shapeErr != nil {
		if _, err := s.log.Append(attestEntry(cn, audit.ResultRefused, "attest_shape", shapeErr.Error(), "", pol)); err != nil {
			s.m.attests.WithLabelValues("audit_error").Inc()
			log.Printf("ALERT audit write failed, attestation request not recorded: %v", err)
			return nil, fmt.Errorf("%w: %v", ErrAudit, err)
		}
		s.m.attests.WithLabelValues("refused").Inc()
		log.Printf("ALERT attest refused caller=%s: %v", cn, shapeErr)
		return nil, shapeErr
	}
	digest := q.Digest()
	dhex := "0x" + hex.EncodeToString(digest[:])
	ent, err := s.log.Append(attestEntry(cn, audit.ResultAuthorized, "attest", "xpub_fp="+q.Fingerprint()+" path="+q.Path+" chain="+q.Chain, dhex, pol))
	if err != nil {
		s.m.attests.WithLabelValues("audit_error").Inc()
		log.Printf("ALERT audit write failed, no attestation produced: %v", err)
		return nil, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	fail := func(cause error) (*AttestResult, error) {
		e := attestEntry(cn, audit.ResultHSMError, "hsm_error", cause.Error(), dhex, pol)
		if _, aerr := s.log.Append(e); aerr != nil {
			log.Printf("audit: could not record hsm_error: %v", aerr)
		}
		s.m.attests.WithLabelValues("hsm_error").Inc()
		return nil, cause
	}
	if err := s.attestor.CheckAttestKey(ctx); err != nil {
		return fail(err)
	}
	rs, err := s.attestor.SignAttestation(ctx, digest[:])
	if err != nil {
		s.m.hsmErrors.WithLabelValues(hsmCode(err)).Inc()
		return fail(err)
	}
	s.m.attests.WithLabelValues("signed").Inc()
	log.Printf("attest signed seq=%d caller=%s chain=%s path=%s xpub_fp=%s", ent.Seq, cn, q.Chain, q.Path, q.Fingerprint())
	return &AttestResult{Statement: string(q.Statement()), Signature: rs, PublicKey: s.attestor.AttestPublicKey(), AuditSeq: ent.Seq}, nil
}

// AttestPublicKey is the attestation public key (nil when the token has none).
func (s *Service) AttestPublicKey() []byte {
	if s.attestor == nil {
		return nil
	}
	return s.attestor.AttestPublicKey()
}
