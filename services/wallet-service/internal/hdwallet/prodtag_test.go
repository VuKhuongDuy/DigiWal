package hdwallet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A `-tags prod` build must not be able to reference the Software Signer
// (key-custody-hsm.md invariant 7): the same probe compiles without the tag and
// fails with it.
func TestProdBuildCannotReferenceSoftwareSigner(t *testing.T) {
	dir, err := os.MkdirTemp(".", "probe")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	src := `package probe

import "digiwal/services/wallet-service/internal/hdwallet"

var _ = hdwallet.FromMnemonic
var _ *hdwallet.Wallet
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(tags string) (string, error) {
		args := []string{"build"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		out, err := exec.Command("go", append(args, "./"+dir)...).CombinedOutput()
		return string(out), err
	}
	if out, err := build(""); err != nil {
		t.Fatalf("probe should compile in the default build: %v\n%s", err, out)
	}
	out, err := build("prod")
	if err == nil {
		t.Fatal("probe referencing the Software Signer compiled under -tags prod")
	}
	if !strings.Contains(out, "undefined") {
		t.Fatalf("expected an undefined-symbol error, got:\n%s", out)
	}
}
