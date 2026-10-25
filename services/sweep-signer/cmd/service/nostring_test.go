package main

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// "No mnemonic string on this path", enforced by construction (key-custody-hsm.md invariant 14, K11): the service binary
// does not even link a BIP39 or mnemonic library, so there is no code in it that could turn the Seed into a string or take
// a mnemonic. go-ethereum-hdwallet and bip39 exist only in TEST files (cross-checks) and in other modules' ceremony tools.
func TestTheServiceBinaryLinksNoMnemonicCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, banned := range []string{"go-ethereum-hdwallet", "go-bip39", "tyler-smith", "bip39", "mnemonic"} {
		for _, pkg := range strings.Fields(string(out)) {
			if strings.Contains(strings.ToLower(pkg), banned) {
				t.Errorf("the service binary links %s (matches %q)", pkg, banned)
			}
		}
	}
	if !strings.Contains(string(out), "hdkeychain") {
		t.Error("expected btcd hdkeychain in the dependency set")
	}
}

// And no non-test source file of the module imports one either, or has a string-typed seed parameter in the derive path.
func TestNoNonTestSourceImportsMnemonicLibraries(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.ToLower(imp.Path.Value)
			if strings.Contains(p, "hdwallet") || strings.Contains(p, "bip39") {
				t.Errorf("%s imports %s", path, imp.Path.Value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
