package signing

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCheckProfile(t *testing.T) {
	cases := []struct {
		name, profile, backend string
		env                    []string
		wantErr                string
	}{
		{"dev software", ProfileDev, BackendSoftware, []string{"MASTER_KEY_HEX=aa"}, ""},
		{"prod software refused", ProfileProd, BackendSoftware, nil, "refuses SIGNER_BACKEND"},
		{"prod empty backend refused", ProfileProd, "", nil, "refuses SIGNER_BACKEND"},
		{"prod pkcs11 clean", ProfileProd, BackendPKCS11, []string{"PATH=/bin"}, ""},
		{"prod master key refused", ProfileProd, BackendPKCS11, []string{"MASTER_KEY_HEX=aa"}, "MASTER_KEY_HEX"},
		{"prod SOFT_ var refused", ProfileProd, BackendPKCS11, []string{"SOFT_SIGNER_KEY=x"}, "SOFT_SIGNER_KEY"},
		{"unknown profile", "staging", BackendSoftware, nil, "unknown SIGNER_PROFILE"},
	}
	for _, c := range cases {
		err := CheckProfile(c.profile, c.backend, c.env)
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.wantErr)
		}
	}
}

// The signing package itself must build with no software backend present.
func TestSigningPackageBuildsUnderProdTag(t *testing.T) {
	if out, err := exec.Command("go", "build", "-tags", "prod", ".").CombinedOutput(); err != nil {
		t.Fatalf("signing does not build with -tags prod: %v\n%s", err, out)
	}
}
