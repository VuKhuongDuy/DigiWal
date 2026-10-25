package hsm

import (
	"errors"
	"fmt"

	"github.com/miekg/pkcs11"
)

// Codes are the gateway's explicit HSM error classes. Every PKCS#11 failure is mapped to one; none
// is ever answered by signing with another key (there is no software path in this service).
const (
	CodeUnavailable  = "hsm_unavailable"    // token/device/session gone or failing: retryable by the caller
	CodeNotLoggedIn  = "hsm_not_logged_in"  // the token-wide login is gone: needs a restart (re-login goes through the PIN guard)
	CodePIN          = "hsm_pin_rejected"   // wrong, locked or expired PIN: never retried
	CodeKeyMissing   = "hsm_key_missing"    // the Hot Key object is not there or not usable
	CodeKeyAttrs     = "hsm_key_attributes" // the key's attributes are not the required ones (not private/sensitive, or extractable)
	CodeBadOutput    = "hsm_bad_output"     // the token returned something that is not a 64-byte r||s
	CodeInternal     = "hsm_internal"       // anything else (including CKR_GENERAL_ERROR, which is not a typed error)
	CodeTokenMissing = "hsm_token_not_found"
)

// Error is a mapped HSM failure.
type Error struct {
	Code  string
	Op    string
	Cause error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s (%s)", e.Code, e.Op)
	}
	return fmt.Sprintf("%s (%s): %v", e.Code, e.Op, e.Cause)
}
func (e *Error) Unwrap() error { return e.Cause }

// ErrPINIncorrect is returned by Open when the token rejects the PIN. The caller must exit and
// must not retry: a restart loop on a wrong PIN is how a real HSM gets locked.
var ErrPINIncorrect = errors.New("hsm: the token rejected the user PIN")

// Map classifies a PKCS#11 error returned by operation op. Non-pkcs11 errors map to hsm_internal.
func Map(op string, err error) *Error {
	if err == nil {
		return nil
	}
	var he *Error
	if errors.As(err, &he) {
		return he
	}
	var pe pkcs11.Error
	if !errors.As(err, &pe) {
		return &Error{Code: CodeInternal, Op: op, Cause: err}
	}
	code := CodeInternal
	switch uint(pe) {
	case pkcs11.CKR_DEVICE_ERROR, pkcs11.CKR_DEVICE_MEMORY, pkcs11.CKR_DEVICE_REMOVED, pkcs11.CKR_TOKEN_NOT_PRESENT,
		pkcs11.CKR_TOKEN_NOT_RECOGNIZED, pkcs11.CKR_SLOT_ID_INVALID, pkcs11.CKR_SESSION_CLOSED,
		pkcs11.CKR_SESSION_HANDLE_INVALID, pkcs11.CKR_SESSION_COUNT, pkcs11.CKR_SESSION_PARALLEL_NOT_SUPPORTED,
		pkcs11.CKR_CRYPTOKI_NOT_INITIALIZED, pkcs11.CKR_FUNCTION_FAILED, pkcs11.CKR_HOST_MEMORY,
		pkcs11.CKR_BUFFER_TOO_SMALL, pkcs11.CKR_OPERATION_ACTIVE:
		code = CodeUnavailable
	case pkcs11.CKR_USER_NOT_LOGGED_IN:
		code = CodeNotLoggedIn
	case pkcs11.CKR_PIN_INCORRECT, pkcs11.CKR_PIN_LOCKED, pkcs11.CKR_PIN_EXPIRED, pkcs11.CKR_PIN_INVALID, pkcs11.CKR_PIN_LEN_RANGE:
		code = CodePIN
	case pkcs11.CKR_KEY_HANDLE_INVALID, pkcs11.CKR_OBJECT_HANDLE_INVALID:
		code = CodeKeyMissing
	case pkcs11.CKR_KEY_FUNCTION_NOT_PERMITTED, pkcs11.CKR_KEY_TYPE_INCONSISTENT, pkcs11.CKR_MECHANISM_INVALID:
		code = CodeKeyAttrs
	}
	return &Error{Code: code, Op: op, Cause: err}
}

// SessionBroken reports whether err means one session handle is dead (the pool re-opens it; the
// token-wide login is unaffected).
func SessionBroken(err error) bool {
	var pe pkcs11.Error
	if !errors.As(err, &pe) {
		return false
	}
	return uint(pe) == pkcs11.CKR_SESSION_HANDLE_INVALID || uint(pe) == pkcs11.CKR_SESSION_CLOSED
}
