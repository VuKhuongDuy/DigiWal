package e2e

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestACC01_AddressIsStableAndSingle(t *testing.T) {
	qa(t)
	u := uid("acc01")
	r1 := do(nil2(), "POST", walletURL+"/v1/users/"+u+"/deposit-address", nil)
	r2 := do(nil2(), "POST", walletURL+"/v1/users/"+u+"/deposit-address", nil)
	r3 := do(nil2(), "GET", walletURL+"/v1/users/"+u+"/deposit-address", nil)
	if r1.Code != 201 || r2.Code != 200 || r3.Code != 200 {
		t.Fatalf("codes %d %d %d", r1.Code, r2.Code, r3.Code)
	}
	if r1.s("address") != r2.s("address") || r1.s("address") != r3.s("address") {
		t.Fatal("address changed between calls")
	}
	if n := q1[int](t, wdb, `SELECT count(*) FROM wallet_addresses WHERE user_id=$1`, u); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// ACC-02: first-ever requests for one user racing each other.
func TestACC02_ConcurrentFirstRequestsForSameUser(t *testing.T) {
	qa(t)
	u := uid("acc02")
	rs := make([]resp, 50)
	parallel(50, func(i int) { rs[i] = do(nil2(), "POST", walletURL+"/v1/users/"+u+"/deposit-address", nil) })
	t.Logf("status codes: %v", codeCount(rs))
	addrs := map[string]bool{}
	for _, r := range rs {
		if r.Code == 200 || r.Code == 201 {
			addrs[r.s("address")] = true
		}
	}
	if len(addrs) != 1 {
		t.Errorf("%d distinct addresses returned for one user", len(addrs))
	}
	if n := q1[int](t, wdb, `SELECT count(*) FROM wallet_addresses WHERE user_id=$1`, u); n != 1 {
		t.Errorf("%d address rows for one user", n)
	}
	if c := codeCount(rs); c[500] > 0 {
		t.Errorf("FINDING A-1: %d of 50 racing first-requests got HTTP 500 (loser of the unique-key race is not retried/read back)", c[500])
	}
}

func TestACC03_DistinctUsersGetUniqueDenseIndices(t *testing.T) {
	qa(t)
	before := q1[int](t, wdb, `SELECT next_index FROM wallet_seeds WHERE seed_role='user_derivation'`)
	users := make([]string, 100)
	rs := make([]resp, 100)
	parallel(100, func(i int) { users[i] = uid("acc03"); rs[i] = do(nil2(), "POST", walletURL+"/v1/users/"+users[i]+"/deposit-address", nil) })
	if c := codeCount(rs); c[201] != 100 {
		t.Fatalf("codes %v", c)
	}
	addr := map[string]bool{}
	for _, r := range rs {
		addr[r.s("address")] = true
	}
	if len(addr) != 100 {
		t.Fatalf("%d unique addresses of 100", len(addr))
	}
	after := q1[int](t, wdb, `SELECT next_index FROM wallet_seeds WHERE seed_role='user_derivation'`)
	if after-before != 100 {
		t.Errorf("next_index advanced by %d for 100 users", after-before)
	}
}

// ACC-04: an address handed to a user must be derivable from the user seed at its recorded index,
// and must never equal the Hot Wallet (else sweeping would be signing as the hot wallet).
func TestACC04_AddressesMatchSeedAndNeverTheHotWallet(t *testing.T) {
	qa(t)
	for i := 0; i < 10; i++ {
		u := uid("acc04")
		a := depositAddress(t, u)
		if a == hotAddr {
			t.Fatalf("user %s got the Hot Wallet address", u)
		}
		var idx int
		var stored string
		if err := wdb.QueryRow(context.Background(), `SELECT derivation_index, address FROM wallet_addresses WHERE user_id=$1`, u).Scan(&idx, &stored); err != nil {
			t.Fatal(err)
		}
		if common.HexToAddress(stored) != a {
			t.Fatalf("stored %s != returned %s", stored, a)
		}
		userKey(t, a) // fatal if not derivable from the user_derivation seed
	}
}

func TestACC06_RedisFlushDoesNotBreakLookups(t *testing.T) {
	qa(t)
	u := uid("acc06")
	a := depositAddress(t, u)
	compose(t, "exec", "-T", "redis", "redis-cli", "FLUSHALL")
	r := ownerOf(a)
	if r.Code != 200 || r.s("user_id") != u {
		t.Fatalf("after FLUSHALL: %d %s", r.Code, r.Body)
	}
	if a2 := depositAddress(t, u); a2 != a {
		t.Fatal("address changed after redis flush")
	}
}

func TestACC08_HostileUserIDs(t *testing.T) {
	qa(t)
	cases := map[string]string{
		"empty":     "",
		"spaces":    "%20%20",
		"nul":       "a%00b",
		"slash":     "a%2Fb",
		"dotdot":    "..",
		"sqli":      url.PathEscape("x'; DROP TABLE wallet_addresses;--"),
		"unicode":   url.PathEscape("người-dùng-💰"),
		"long10k":   strings.Repeat("a", 10000),
		"newline":   "a%0Ab",
		"percent":   "%25",
	}
	for name, id := range cases {
		r := do(nil2(), "POST", walletURL+"/v1/users/"+id+"/deposit-address", nil)
		t.Logf("%-8s -> %d", name, r.Code)
		if r.Code >= 500 {
			t.Errorf("FINDING A-2: user id %q gives HTTP %d: %s", name, r.Code, r.Body)
		}
	}
	// whitespace is trimmed server-side: "bob" and " bob" are one user; case is significant.
	u := uid("Acc08")
	a1 := depositAddress(t, u)
	a2 := depositAddress(t, "%20"+u)
	a3 := depositAddress(t, strings.ToLower(u))
	if a1 != a2 {
		t.Errorf("leading space made a different user")
	}
	if a1 == a3 && u != strings.ToLower(u) {
		t.Errorf("case variants collapsed into one user")
	}
	if n := q1[int](t, wdb, `SELECT count(*) FROM wallet_addresses WHERE user_id LIKE '%DROP%'`); n > 1 {
		t.Errorf("odd rows %d", n)
	}
}

// ACC-14: ledger side — many first deposits for a brand-new user create one account, correct sum.
func TestACC14_ConcurrentFirstDepositsCreateOneAccount(t *testing.T) {
	qa(t)
	u := uid("acc14")
	var evs []depositEvent
	for i := 0; i < 20; i++ {
		evs = append(evs, depositEvent{1, chainID, fmt.Sprintf("0x%064x", 0xacc14000+i+int(uniq.Add(1))*1000), -1, ethAsset,
			"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "5", 1})
	}
	parallel(4, func(i int) { publish(t, evs[i*5:(i+1)*5]...) })
	waitCredited(t, u, ethAsset, 100)
	if n := count(t, `SELECT count(*) FROM accounts WHERE user_id=$1 AND asset_id=$2`, u, ethAsset); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}
