package screening

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "screening.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndContains(t *testing.T) {
	l, err := Load(write(t, `addresses: ["0x70997970C51812dc3A010C7d01b50e0d17dc79C8"]`))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"} {
		if !l.Contains(a) {
			t.Errorf("%s should match regardless of case", a)
		}
	}
	if l.Contains("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC") {
		t.Error("unlisted address matched")
	}
}

func TestEmptyListIsValid(t *testing.T) {
	for _, body := range []string{`addresses: []`, ``} {
		l, err := Load(write(t, body))
		if err != nil || l.Len() != 0 {
			t.Fatalf("%q: %v", body, err)
		}
	}
}

func TestMalformedEntriesFailStartup(t *testing.T) {
	for _, body := range []string{`addresses: ["0x123"]`, `addresses: ["alice"]`, `addresses: [`} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
	if _, err := Load("/nonexistent"); err == nil {
		t.Error("missing file accepted")
	}
}

func TestCanonicalListLoads(t *testing.T) {
	if _, err := Load("../../../../infra/screening.yaml"); err != nil {
		t.Fatal(err)
	}
}
