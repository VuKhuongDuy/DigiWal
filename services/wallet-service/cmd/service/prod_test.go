package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// key-custody-hsm.md M3b, invariants 9 and 23: the production wallet-service has no seed and no env key. A `-tags prod` build of
// this command compiles (the software signer, the sealer and every seed import are behind `!prod`) and links no mnemonic,
// BIP39 or master-key code: not the software signer, not the Sealer that reads MASTER_KEY_HEX, not key-admin's logic.
func TestProdBuildCompilesAndLinksNoSeedOrMasterKeyCode(t *testing.T) {
	if out, err := exec.Command("go", "build", "-tags", "prod", "-o", "/dev/null", ".").CombinedOutput(); err != nil {
		t.Fatalf("go build -tags prod ./cmd/service:\n%s", out)
	}
	out, err := exec.Command("go", "list", "-tags", "prod", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, banned := range []string{"go-ethereum-hdwallet", "go-bip39", "wallet-service/internal/crypto", "wallet-service/internal/keyadmin"} {
			if strings.Contains(pkg, banned) {
				t.Errorf("a prod build of wallet-service links %s (matches %q)", pkg, banned)
			}
		}
	}
}

// The service's own main.go has no code that handles the master key or the user_derivation role (comments may mention them).
func TestMainGoHasNoMasterKeyOrUserSeedCode(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for n, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, banned := range []string{"NewSealer", "RoleUserDerivation", "MASTER_KEY_HEX"} {
			if strings.Contains(line, banned) {
				t.Errorf("main.go:%d handles %s: %s", n+1, banned, strings.TrimSpace(line))
			}
		}
	}
}
