package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// `sim run`: a daemon that behaves like a population of users, forever. Each tick (Poisson arrivals)
// it picks an action — deposit, withdraw, check balances, sign up, retry a request — and runs it in
// its own goroutine. It also keeps, per user, what the ledger *should* say (deposits sent minus
// withdrawals confirmed) and reconciles it against api-gateway once the user has gone quiet, so a
// long run is a continuous end-to-end check of deposit -> ledger -> withdraw and not just load.

const (
	minWithdrawETH = 1e15 // assets.yaml min_withdrawal_amount is 6e14 / 1e18; stay clear of the edge
	minWithdrawTT  = 1e18
	quietFor       = 90 * time.Second // a user is reconciled only after this long without an operation
	maxDests       = 5
)

type simulator struct {
	st    *state
	c     *chain
	log   *slog.Logger
	users int // population cap
	stats struct {
		deposits, withdrawals, confirmed, rejected, balanceChecks, signups, retries, skipped, errors, drift atomic.Int64
	}
	confirmLatency atomic.Int64 // sum of ms, over `confirmed`
	sem            chan struct{}
	wg             *sync.WaitGroup
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	users := fs.Int("users", envInt("SIM_USERS", 25), "population cap (users sign up gradually until it is reached)")
	rate := fs.Float64("rate", float64(envInt("SIM_RATE", 12)), "average actions per minute across all users")
	workers := fs.Int("workers", 8, "max actions in flight; ticks beyond that are skipped")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Anvil, the gateway and wallet-service may still be starting (e.g. after a VM reboot).
	var c *chain
	for {
		var err error
		if c, err = dial(ctx); err == nil {
			if err = c.ensureReady(ctx, false); err == nil {
				break
			}
		}
		log.Warn("stack not ready, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	sim := &simulator{st: st, c: c, log: log, users: *users, sem: make(chan struct{}, *workers), wg: &sync.WaitGroup{}}
	sim.checkStateMatchesStack(ctx)
	log.Info("simulator started", "users_cap", *users, "actions_per_min", *rate, "known_users", len(st.Users), "state", stateFile())

	// Resume tracking withdrawals that were accepted before a restart.
	for _, u := range st.Users {
		for _, p := range u.Pending {
			sim.goRun(ctx, func() { sim.track(ctx, u, p) })
		}
	}
	for len(st.Users) < min(5, *users) {
		if err := sim.signup(ctx); err != nil {
			log.Warn("initial signup failed", "err", err)
			break
		}
	}

	go sim.background(ctx)
	mean := time.Duration(float64(time.Minute) / *rate)
	for {
		wait := time.Duration(rand.ExpFloat64() * float64(mean))
		select {
		case <-ctx.Done():
			log.Info("stopping, waiting for in-flight actions")
			sim.wg.Wait()
			sim.report()
			return nil
		case <-time.After(wait):
		}
		select {
		case sim.sem <- struct{}{}:
			sim.goRun(ctx, func() { sim.act(ctx) })
		default:
			sim.stats.skipped.Add(1)
		}
	}
}

func envInt(k string, def int) int {
	var n int
	if _, err := fmt.Sscan(os.Getenv(k), &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// goRun runs f as a tracked goroutine, releasing a worker slot if it holds one.
func (s *simulator) goRun(ctx context.Context, f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// checkStateMatchesStack forgets saved users when the stack was wiped under us (`make down` drops the
// database and so every API key), instead of failing forever with 401s.
func (s *simulator) checkStateMatchesStack(ctx context.Context) {
	if len(s.st.Users) == 0 {
		return
	}
	if _, err := s.st.Users[0].balances(ctx); err != nil && strings.Contains(err.Error(), "401") {
		s.log.Warn("saved users are unknown to the stack (it was reset): starting over")
		s.st.mutate(func() { s.st.Users = nil })
	}
}

// ---- action choice --------------------------------------------------------------------

func (s *simulator) act(ctx context.Context) {
	defer func() { <-s.sem }()
	type choice struct {
		weight int
		name   string
		fn     func(context.Context) error
	}
	choices := []choice{
		{35, "deposit", s.deposit},
		{25, "withdraw", s.withdraw},
		{28, "balances", s.checkBalances},
		{4, "retry", s.retryWithdrawal},
	}
	if s.userCount() < s.users {
		choices = append(choices, choice{8, "signup", s.signup})
	}
	total := 0
	for _, ch := range choices {
		total += ch.weight
	}
	n := rand.IntN(total)
	for _, ch := range choices {
		if n -= ch.weight; n < 0 {
			if err := ch.fn(ctx); err != nil && ctx.Err() == nil {
				s.stats.errors.Add(1)
				s.log.Error("action failed", "action", ch.name, "err", err)
			}
			return
		}
	}
}

func (s *simulator) userCount() int {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	return len(s.st.Users)
}

// pick returns a random user, whales and regulars more often than lurkers.
func (s *simulator) pick(want func(*user) bool) *user {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	total := 0
	for _, u := range s.st.Users {
		if want == nil || want(u) {
			total += activity(u.Profile)
		}
	}
	if total == 0 {
		return nil
	}
	n := rand.IntN(total)
	for _, u := range s.st.Users {
		if want == nil || want(u) {
			if n -= activity(u.Profile); n < 0 {
				return u
			}
		}
	}
	return nil
}

func activity(profile string) int {
	switch profile {
	case "whale":
		return 2
	case "lurker":
		return 1
	}
	return 4
}

// ---- actions --------------------------------------------------------------------------

func (s *simulator) signup(ctx context.Context) error {
	profile := "regular"
	switch n := rand.IntN(10); {
	case n == 0:
		profile = "whale"
	case n >= 7:
		profile = "lurker"
	}
	s.st.mu.Lock()
	name := ""
	for k := len(s.st.Users) + 1; ; k++ {
		if name = fmt.Sprintf("u%04d", k); !s.st.has(name) {
			break
		}
	}
	s.st.mu.Unlock()
	u, err := registerUser(ctx, name)
	if err != nil {
		return err
	}
	u.Profile = profile
	s.st.mutate(func() { s.st.Users = append(s.st.Users, u) })
	s.stats.signups.Add(1)
	s.log.Info("signup", "user", u.Name, "profile", profile, "id", u.ID, "deposit_address", u.Deposit)
	return nil
}

func (s *simulator) deposit(ctx context.Context) error {
	u := s.pick(nil)
	if u == nil {
		return s.signup(ctx)
	}
	a := assets["eth"]
	if rand.IntN(10) >= 7 {
		a = assets["tt"]
	}
	amt := randomAmount(a, u.Profile)
	to := common.HexToAddress(u.Deposit)
	var err error
	var txHash common.Hash
	if a.id == ethAssetID {
		r, e := s.c.send(ctx, s.c.key1, &to, amt, nil)
		err = e
		if e == nil {
			txHash = r.TxHash
		}
	} else {
		r, e := s.c.send(ctx, s.c.key0, &s.c.token, big.NewInt(0), transferCalldata(to, amt))
		err = e
		if e == nil {
			txHash = r.TxHash
		}
	}
	if err != nil {
		return fmt.Errorf("deposit %s %s to %s: %w", fmtAmount(amt.String()), a.symbol, u.Name, err)
	}
	// Only once it is mined is it owed: the ledger credit follows after the Confirmation Depth.
	s.st.mutate(func() { u.addExpected(a.id, amt); u.LastOp = time.Now() })
	s.stats.deposits.Add(1)
	s.log.Info("deposit", "user", u.Name, "asset", a.symbol, "amount", fmtAmount(amt.String()), "tx", txHash)
	return nil
}

func (s *simulator) withdraw(ctx context.Context) error {
	u := s.pick(func(u *user) bool { return !u.busy })
	if u == nil {
		s.stats.skipped.Add(1)
		return nil
	}
	s.st.mutate(func() { u.busy = true })
	release := func() { s.st.mutate(func() { u.busy = false }) }
	defer release()

	a := assets["eth"]
	if rand.IntN(10) >= 7 {
		a = assets["tt"]
	}
	b, err := u.balances(ctx)
	if err != nil {
		return err
	}
	avail := bigOf(b[a.id].Available)
	floor := big.NewInt(minWithdrawETH)
	if a.id == tokenAssetID {
		floor = big.NewInt(minWithdrawTT)
	}
	// Now and then a user asks for more than they have: the request must be rejected, balance untouched.
	overdraw := rand.IntN(20) == 0
	amt := new(big.Int).Div(new(big.Int).Mul(avail, big.NewInt(int64(20+rand.IntN(71)))), big.NewInt(100))
	amt.Sub(amt, new(big.Int).Mod(amt, big.NewInt(1e14))) // 4 decimals
	if overdraw {
		amt = new(big.Int).Add(new(big.Int).Mul(avail, big.NewInt(2)), floor)
	} else if amt.Cmp(floor) < 0 {
		s.stats.skipped.Add(1)
		return nil
	}
	dest, err := s.destination(ctx, u)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("sim-%d-%04d", time.Now().UnixNano(), rand.IntN(10000))
	code, body, err := u.do(ctx, "POST", "/v1/withdrawals", map[string]string{
		"idempotency_key": key, "asset_id": a.id, "amount": amt.String(), "to_address": dest.Hex()})
	if err != nil {
		return err
	}
	s.stats.withdrawals.Add(1)
	s.st.mutate(func() { u.LastOp = time.Now() })
	switch {
	case code == 422 && overdraw:
		s.stats.rejected.Add(1)
		s.log.Info("withdrawal rejected as expected", "user", u.Name, "asset", a.symbol, "amount", fmtAmount(amt.String()), "response", strings.TrimSpace(string(body)))
		return nil
	case code == 202 || code == 200:
		if overdraw {
			s.log.Error("ANOMALY: an overdraw was accepted", "user", u.Name, "asset", a.symbol, "available", fmtAmount(avail.String()), "asked", fmtAmount(amt.String()))
		}
	case code == 422: // e.g. a limit this simulator does not model: noted, not an error
		s.stats.rejected.Add(1)
		s.log.Warn("withdrawal rejected", "user", u.Name, "asset", a.symbol, "amount", fmtAmount(amt.String()), "response", strings.TrimSpace(string(body)))
		return nil
	default:
		return fmt.Errorf("POST /v1/withdrawals: %d %s", code, body)
	}
	p := pendingWithdrawal{Key: key, Asset: a.id, Amount: amt.String(), To: dest.Hex(), Since: time.Now()}
	s.st.mutate(func() { u.Pending = append(u.Pending, p) })
	s.log.Info("withdrawal accepted", "user", u.Name, "asset", a.symbol, "amount", fmtAmount(amt.String()), "to", dest, "key", key)
	s.track(ctx, u, p) // keeps this user busy until it settles
	return nil
}

// destination returns an active whitelisted address: usually one the user used before, sometimes a new one.
func (s *simulator) destination(ctx context.Context, u *user) (common.Address, error) {
	s.st.mu.Lock()
	known := append([]string(nil), u.Dests...)
	s.st.mu.Unlock()
	if len(known) > 0 && (len(known) >= maxDests || rand.IntN(10) < 7) {
		return common.HexToAddress(known[rand.IntN(len(known))]), nil
	}
	k, _ := crypto.GenerateKey()
	dest := crypto.PubkeyToAddress(k.PublicKey)
	if _, err := u.mustDo(ctx, "POST", "/v1/whitelist", map[string]string{"address": dest.Hex()}, 201); err != nil {
		return dest, err
	}
	err := waitFor(ctx, 5*time.Minute, "the whitelist cooldown", func() (bool, string, error) {
		st, err := u.whitelistStatus(ctx, dest)
		return st == "active", st, err
	}, func() {})
	if err != nil {
		return dest, err
	}
	s.st.mutate(func() { u.Dests = append(u.Dests, dest.Hex()) })
	return dest, nil
}

// track polls one accepted withdrawal until it is confirmed (or ends badly), then settles the books.
func (s *simulator) track(ctx context.Context, u *user, p pendingWithdrawal) {
	for ctx.Err() == nil {
		body, err := u.mustDo(ctx, "GET", "/v1/withdrawals/"+p.Key, nil, 200)
		if err == nil {
			var r struct {
				Status string  `json:"status"`
				TxHash *string `json:"tx_hash"`
			}
			if json.Unmarshal(body, &r) == nil {
				switch r.Status {
				case "confirmed":
					if r.TxHash != nil {
						s.finish(u, p, true, "")
						s.stats.confirmed.Add(1)
						ms := time.Since(p.Since).Milliseconds()
						s.confirmLatency.Add(ms)
						s.log.Info("withdrawal confirmed", "user", u.Name, "key", p.Key, "tx", *r.TxHash, "took", time.Since(p.Since).Round(time.Millisecond))
						return
					}
				case "failed", "rejected", "disputed":
					// Funds were released (or are for an operator to resolve): the expectation is unchanged
					// for failed/rejected; a dispute is flagged loudly and the user is left out of reconciliation.
					s.finish(u, p, false, r.Status)
					s.log.Error("withdrawal ended badly", "user", u.Name, "key", p.Key, "status", r.Status, "body", strings.TrimSpace(string(body)))
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *simulator) finish(u *user, p pendingWithdrawal, paid bool, status string) {
	s.st.mutate(func() {
		for i, q := range u.Pending {
			if q.Key == p.Key {
				u.Pending = append(u.Pending[:i], u.Pending[i+1:]...)
				break
			}
		}
		if paid {
			u.subExpected(p.Asset, bigOf(p.Amount))
		}
		if status == "disputed" {
			u.Expected = nil // unknowable until an operator resolves it
		}
		u.busy = false
		u.LastOp = time.Now()
	})
}

// retryWithdrawal re-sends a pending request with the same idempotency key and body, as a client that timed
// out would: the gateway must treat it as the same request, never as a second payout.
func (s *simulator) retryWithdrawal(ctx context.Context) error {
	s.st.mu.Lock()
	var u *user
	var p pendingWithdrawal
	for _, c := range s.st.Users {
		if len(c.Pending) > 0 && c.Pending[0].To != "" {
			u, p = c, c.Pending[0]
			break
		}
	}
	s.st.mu.Unlock()
	if u == nil {
		s.stats.skipped.Add(1)
		return nil
	}
	code, body, err := u.do(ctx, "POST", "/v1/withdrawals", map[string]string{
		"idempotency_key": p.Key, "asset_id": p.Asset, "amount": p.Amount, "to_address": p.To})
	if err != nil {
		return err
	}
	s.stats.retries.Add(1)
	if code != 200 && code != 202 {
		s.log.Error("ANOMALY: a retried withdrawal was not recognised as the same request", "user", u.Name, "key", p.Key, "code", code, "body", strings.TrimSpace(string(body)))
	} else {
		s.log.Info("withdrawal retried, same request", "user", u.Name, "key", p.Key, "code", code)
	}
	return nil
}

func (s *simulator) checkBalances(ctx context.Context) error {
	u := s.pick(nil)
	if u == nil {
		return nil
	}
	b, err := u.balances(ctx)
	if err != nil {
		return err
	}
	s.stats.balanceChecks.Add(1)
	for id, v := range b {
		tot, lk, av := bigOf(v.Total), bigOf(v.Locked), bigOf(v.Available)
		if tot.Sign() < 0 || lk.Sign() < 0 || av.Sign() < 0 || new(big.Int).Add(lk, av).Cmp(tot) != 0 {
			s.log.Error("ANOMALY: inconsistent balance", "user", u.Name, "asset", id, "total", v.Total, "locked", v.Locked, "available", v.Available)
		}
	}
	return nil
}

// ---- reconciliation, upkeep, reporting ------------------------------------------------

func (s *simulator) background(ctx context.Context) {
	report := time.NewTicker(time.Minute)
	upkeep := time.NewTicker(2 * time.Minute)
	defer report.Stop()
	defer upkeep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-report.C:
			s.reconcile(ctx)
			s.report()
		case <-upkeep.C:
			if err := s.c.ensureReady(ctx, false); err != nil && ctx.Err() == nil {
				s.log.Error("upkeep failed", "err", err)
			}
		}
	}
}

// reconcile compares each quiet user's ledger total with what the simulator has put in and taken out.
func (s *simulator) reconcile(ctx context.Context) {
	s.st.mu.Lock()
	var quiet []*user
	for _, u := range s.st.Users {
		if !u.busy && len(u.Pending) == 0 && u.Expected != nil && time.Since(u.LastOp) > quietFor {
			quiet = append(quiet, u)
		}
	}
	s.st.mu.Unlock()
	checked := 0
	for _, u := range quiet {
		b, err := u.balances(ctx)
		if err != nil {
			continue
		}
		s.st.mu.Lock()
		exp := map[string]string{}
		for k, v := range u.Expected {
			exp[k] = v
		}
		stillQuiet := !u.busy && len(u.Pending) == 0 && time.Since(u.LastOp) > quietFor
		s.st.mu.Unlock()
		if !stillQuiet {
			continue
		}
		checked++
		for _, a := range assets {
			if got, want := bigOf(b[a.id].Total), bigOf(exp[a.id]); got.Cmp(want) != 0 {
				s.stats.drift.Add(1)
				s.log.Error("DRIFT: ledger does not match deposits minus withdrawals", "user", u.Name, "asset", a.symbol,
					"expected", fmtAmount(want.String()), "ledger", fmtAmount(got.String()))
			}
		}
	}
	s.log.Info("reconciled", "quiet_users", checked)
}

func (s *simulator) report() {
	avg := time.Duration(0)
	if n := s.stats.confirmed.Load(); n > 0 {
		avg = time.Duration(s.confirmLatency.Load()/n) * time.Millisecond
	}
	s.log.Info("stats", "users", s.userCount(), "deposits", s.stats.deposits.Load(), "withdrawals", s.stats.withdrawals.Load(),
		"confirmed", s.stats.confirmed.Load(), "rejected", s.stats.rejected.Load(), "avg_confirm", avg.Round(time.Millisecond),
		"balance_checks", s.stats.balanceChecks.Load(), "signups", s.stats.signups.Load(), "skipped", s.stats.skipped.Load(),
		"errors", s.stats.errors.Load(), "drift", s.stats.drift.Load())
}

// ---- helpers --------------------------------------------------------------------------

func (s *state) mutate(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	if err := s.saveLocked(); err != nil {
		fmt.Fprintln(os.Stderr, "saving state:", err)
	}
}

func (u *user) addExpected(asset string, amt *big.Int) {
	if u.Expected == nil {
		u.Expected = map[string]string{}
	}
	u.Expected[asset] = new(big.Int).Add(bigOf(u.Expected[asset]), amt).String()
}

func (u *user) subExpected(asset string, amt *big.Int) {
	if u.Expected == nil {
		return
	}
	u.Expected[asset] = new(big.Int).Sub(bigOf(u.Expected[asset]), amt).String()
}

// registerUser is POST /users plus fetching the deposit address.
func registerUser(ctx context.Context, name string) (*user, error) {
	body, err := postJSON(ctx, gatewayURL+"/users", "{}", 201)
	if err != nil {
		return nil, err
	}
	var r struct {
		UserID string `json:"user_id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	u := &user{Name: name, ID: r.UserID, APIKey: r.APIKey, Expected: map[string]string{}}
	if _, err := u.depositAddress(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// randomAmount draws a plausible amount: many small, a few large, whales ten times bigger. 4 decimals.
func randomAmount(a asset, profile string) *big.Int {
	mean, lo, hi := 0.3, 0.002, 3.0 // ETH
	if a.id == tokenAssetID {
		mean, lo, hi = 60.0, 2.0, 600.0
	}
	switch profile {
	case "whale":
		mean, hi = mean*8, hi*8
	case "lurker":
		mean, hi = mean/3, hi/3
	}
	x := math.Min(math.Max(rand.ExpFloat64()*mean, lo), hi)
	units := int64(math.Round(x * 1e4))
	if units < 1 {
		units = 1
	}
	return new(big.Int).Mul(big.NewInt(units), big.NewInt(1e14))
}
