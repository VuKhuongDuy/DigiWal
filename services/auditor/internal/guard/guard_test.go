package guard

import "testing"

func TestCheck(t *testing.T) {
	allowed := []int64{31337, 11155111}
	cases := []struct {
		name    string
		env     string
		chain   int64
		wantErr bool
	}{
		{"dev on local anvil", "dev", 31337, false},
		{"dev on test network", "dev", 11155111, false},
		{"mainnet is refused even with env dev", "dev", 1, true},
		{"env unset", "", 31337, true},
		{"env prod", "prod", 31337, true},
		{"env is case sensitive", "DEV", 31337, true},
	}
	for _, c := range cases {
		err := Check(c.env, c.chain, allowed)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestParseChainIDs(t *testing.T) {
	got, err := ParseChainIDs(" 31337, 11155111 ,")
	if err != nil || len(got) != 2 || got[0] != 31337 || got[1] != 11155111 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", " , ", "abc", "-1", "0"} {
		if _, err := ParseChainIDs(bad); err == nil {
			t.Errorf("ParseChainIDs(%q) should fail", bad)
		}
	}
}
