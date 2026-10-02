package chaos

import (
	"context"
	"testing"
	"time"
)

// C1: smoke — deposit, wait credited, withdraw half, confirmed, paid once, ledger + chain agree.
func TestC1_DepositThenWithdraw(t *testing.T) {
	qa(t)
	u := newUser(t)
	t.Logf("user %s deposit address %s", u.id, u.dep)
	u.fundAndCredit(t, ethAsset, eth1(2))
	dest := fresh()
	u.whitelist(t, dest)
	code, body := u.withdraw("c1-"+u.id, ethAsset, eth1(1), dest)
	if code != 202 && code != 200 && code != 201 {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	st := u.settleAll(t, []string{"c1-" + u.id}, 120*time.Second)
	if st["c1-"+u.id] != "confirmed" {
		t.Fatalf("status %v", st)
	}
	if got := ethBal(dest); got.Cmp(eth1(1)) != 0 {
		t.Fatalf("destination has %s", got)
	}
	b := u.bal(t, ethAsset)
	if b.Total.Cmp(eth1(1)) != 0 || b.Locked.Sign() != 0 {
		t.Fatalf("ledger total/locked %s/%s, want 1 ETH / 0", b.Total, b.Locked)
	}
	_ = context.Background()
}
