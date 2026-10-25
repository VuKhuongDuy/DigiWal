package signing

import (
	"fmt"
	"os"
	"strings"
)

// Signer profiles (key-custody-hsm.md invariant 7).
const (
	ProfileDev  = "dev"
	ProfileProd = "prod"

	BackendSoftware = "software"
	BackendPKCS11   = "pkcs11"
)

// CheckProfile is the startup check for the signer profile. In the prod profile it
// refuses a non-pkcs11 backend, MASTER_KEY_HEX, and any SOFT_* variable. environ
// is os.Environ() form ("K=V"). The dev profile accepts anything known.
func CheckProfile(profile, backend string, environ []string) error {
	switch profile {
	case ProfileDev:
		return nil
	case ProfileProd:
	default:
		return fmt.Errorf("unknown SIGNER_PROFILE %q (want %q or %q)", profile, ProfileDev, ProfileProd)
	}
	if backend != BackendPKCS11 {
		return fmt.Errorf("SIGNER_PROFILE=prod refuses SIGNER_BACKEND=%q (only %q)", backend, BackendPKCS11)
	}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "MASTER_KEY_HEX" || strings.HasPrefix(k, "SOFT_") {
			return fmt.Errorf("SIGNER_PROFILE=prod refuses %s in the environment", k)
		}
	}
	return nil
}

// ProfileFromEnv reads SIGNER_PROFILE (default dev) and SIGNER_BACKEND (default
// software) and runs CheckProfile on the process environment.
func ProfileFromEnv() error {
	profile, backend := os.Getenv("SIGNER_PROFILE"), os.Getenv("SIGNER_BACKEND")
	if profile == "" {
		profile = ProfileDev
	}
	if backend == "" {
		backend = BackendSoftware
	}
	return CheckProfile(profile, backend, os.Environ())
}
