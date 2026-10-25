package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPins(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	write := func(s string) string {
		p := filepath.Join(t.TempDir(), "callers.yaml")
		_ = os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	if p, err := LoadPins(write("callers:\n  - cn: wallet-service\n    sha256: \"" + fp + "\"\n")); err != nil || p.byFP[fp] != "wallet-service" {
		t.Fatalf("good file: %v", err)
	}
	// openssl prints colon-separated upper-case fingerprints; accept them
	colon := strings.ToUpper(strings.Join(strings.SplitAfter(fp, "ab"), ":"))
	colon = strings.TrimSuffix(colon, ":")
	if _, err := LoadPins(write("callers:\n  - cn: sweep-signer\n    sha256: \"" + colon + "\"\n")); err != nil {
		t.Fatalf("openssl format: %v", err)
	}
	for name, y := range map[string]string{
		"empty list":  "callers: []\n",
		"short hash":  "callers:\n  - cn: a\n    sha256: abcd\n",
		"no cn":       "callers:\n  - sha256: \"" + fp + "\"\n",
		"unknown key": "callers:\n  - cn: a\n    sha256: \"" + fp + "\"\n    trust_all: true\n",
		"not hex":     "callers:\n  - cn: a\n    sha256: \"" + strings.Repeat("zz", 32) + "\"\n",
	} {
		if _, err := LoadPins(write(y)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadPins(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}
