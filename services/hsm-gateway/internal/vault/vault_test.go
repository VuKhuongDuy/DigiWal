package vault

import (
	"strings"
	"testing"
)

func TestAAD_GoldenAndBindsEveryField(t *testing.T) {
	w := []byte("wrapped-dek-bytes")
	got := string(AAD("user_derivation", "sweep", 1, 1, w))
	if !strings.HasPrefix(got, "user_derivation|sweep|1|1|") || len(got) != len("user_derivation|sweep|1|1|")+64 {
		t.Fatalf("aad %q", got)
	}
	base := string(AAD("user_derivation", "sweep", 1, 1, w))
	for name, other := range map[string]string{
		"role":       string(AAD("other", "sweep", 1, 1, w)),
		"purpose":    string(AAD("user_derivation", "x", 1, 1, w)),
		"version":    string(AAD("user_derivation", "sweep", 2, 1, w)),
		"kek":        string(AAD("user_derivation", "sweep", 1, 2, w)),
		"wrapped":    string(AAD("user_derivation", "sweep", 1, 1, []byte("other"))),
		"separators": string(AAD("user_derivation|sweep", "1", 1, 1, w)),
	} {
		if other == base {
			t.Errorf("AAD does not depend on the %s", name)
		}
	}
}

func TestDecode_IsStrict(t *testing.T) {
	for name, body := range map[string]string{"unknown field": `{"role":"a","extra":1}`, "trailing": `{"role":"a"} {"role":"b"}`, "not json": `x`} {
		if _, err := DecodeRow(strings.NewReader(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
		if _, err := DecodeSeal(strings.NewReader(body)); err == nil {
			t.Errorf("%s accepted by DecodeSeal", name)
		}
	}
	if r, err := DecodeRow(strings.NewReader(`{"role":"user_derivation","purpose":"sweep","version":1,"kek_version":1,"wrapped_dek":"AQI=","nonce":"AQ==","ciphertext":"Ag==","aad":"Aw=="}`)); err != nil || len(r.WrappedDEK) != 2 {
		t.Fatalf("%v %v", r, err)
	}
}
