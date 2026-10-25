package gateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"
	"github.com/prometheus/client_golang/prometheus"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/intent"
	"digiwal/services/hsm-gateway/internal/policy"
)

// softBackend is a TEST-ONLY stand-in for the token (it lives in a _test file: the service binary
// contains no software signing path). It returns raw r||s like CKM_ECDSA, so about half of its
// signatures have a high s.
type softBackend struct {
	key      *ecdsa.PrivateKey
	calls    atomic.Int64
	failWith error                  // returned by SignDigest when set
	checkErr error                  // returned by CheckKey when set
	onSign   func(digest []byte)    // runs before signing (ordering assertions, crash hooks)
	mangle   func(rs []byte) []byte // corrupts the output
}

func newSoft(t testing.TB) *softBackend {
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &softBackend{key: k}
}
func (s *softBackend) Address() common.Address { return crypto.PubkeyToAddress(s.key.PublicKey) }
func (s *softBackend) PublicKey() []byte       { return crypto.FromECDSAPub(&s.key.PublicKey) }
func (s *softBackend) Close()                  {}
func (s *softBackend) CheckKey(context.Context) error {
	return s.checkErr
}
func (s *softBackend) SignDigest(_ context.Context, digest []byte) ([]byte, error) {
	s.calls.Add(1)
	if s.onSign != nil {
		s.onSign(digest)
	}
	if s.failWith != nil {
		return nil, s.failWith
	}
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, digest)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sv.FillBytes(out[32:])
	if s.mangle != nil {
		out = s.mangle(out)
	}
	return out, nil
}

func policyFor(hot common.Address, chainID uint64) string {
	return fmt.Sprintf(`
enabled: true
chain_id: %d
hot_address: "%s"
warm_address: "0x2222222222222222222222222222222222222222"
rate_per_minute: 100000
native:
  max_per_tx: "1000000000000000000"
  treasury_max_per_tx: "50000000000000000000"
  window_1h: "5000000000000000000"
  window_24h: "10000000000000000000"
`, chainID, hot.Hex())
}

type fixture struct {
	svc     *Service
	be      hsm.Backend
	log     *audit.Log
	logPath string
	pol     *policy.Policy
}

func newFixture(t *testing.T, be hsm.Backend, chainID uint64) *fixture {
	t.Helper()
	pol, err := policy.Parse([]byte(policyFor(be.Address(), chainID)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audit", "audit.log")
	l, opened, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	svc, err := New(pol, l, opened.Entries, be, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{svc: svc, be: be, log: l, logPath: path, pol: pol}
}

func payout(chainID, nonce uint64, to string, value int64) *intent.Intent {
	return &intent.Intent{ChainID: chainID, Nonce: nonce, To: common.HexToAddress(to), Value: big.NewInt(value), Gas: 21000,
		MaxFee: big.NewInt(10_000_000_000), Tip: big.NewInt(1_000_000_000), Operation: intent.OpPayout, IdempotencyKey: fmt.Sprintf("k%d", nonce)}
}

const user = "0x5555555555555555555555555555555555555555"

func entries(t *testing.T, path string) []audit.Entry {
	t.Helper()
	l, op, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	return op.Entries
}

// ---- happy path, ordering, refusal

func TestSign_AuditEntryIsDurableBeforeTheHSMIsAskedAndBeforeAnythingIsReturned(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	be.onSign = func(digest []byte) {
		// At the moment the token is called, the authorizing entry must already be in the file (fsynced).
		found := false
		for _, e := range entries(t, f.logPath) {
			if e.Digest == "0x"+hex.EncodeToString(digest) && e.Result == audit.ResultAuthorized && e.Op == audit.OpHotSign {
				found = true
			}
		}
		if !found {
			t.Error("the HSM was called before the audit entry for this digest was durable")
		}
	}
	res, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1000))
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(res.RawTx); err != nil {
		t.Fatal(err)
	}
	from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(1337)), &tx)
	if err != nil || from != be.Address() || res.From != from || res.TxHash != tx.Hash() {
		t.Fatalf("sender %v err %v", from, err)
	}
	if _, v, _ := tx.RawSignatureValues(); v.Sign() < 0 { // type-2: v is the recovery id
		t.Fatal("bad v")
	}
	_, _, s := tx.RawSignatureValues()
	if s.Cmp(new(big.Int).Rsh(crypto.S256().Params().N, 1)) > 0 {
		t.Fatal("returned a high-s signature")
	}
	e := entries(t, f.logPath)[0]
	if e.CallerCN != "wallet-service" || e.PolicyHash != f.pol.Hash || e.Rule != policy.AcceptPayout || e.Idempotency != "k0" || e.Seq != res.AuditSeq {
		t.Fatalf("audit entry: %+v", e)
	}
}

func TestSign_RefusalIsAuditedAndNeverReachesTheHSM(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	in := payout(1337, 0, user, 1000)
	in.Data = append([]byte{0x09, 0x5e, 0xa7, 0xb3}, make([]byte, 64)...) // approve
	_, err := f.svc.Sign(context.Background(), "wallet-service", in)
	var pr *PolicyRefused
	if !errors.As(err, &pr) || pr.Rule != policy.RuleApproveRefused {
		t.Fatalf("got %v", err)
	}
	if be.calls.Load() != 0 {
		t.Fatal("the HSM was called for a refused intent")
	}
	es := entries(t, f.logPath)
	if len(es) != 1 || es[0].Result != audit.ResultRefused || es[0].Rule != policy.RuleApproveRefused {
		t.Fatalf("entries: %+v", es)
	}
}

func TestSign_NoSignatureWhenTheAuditLogCannotBeWritten(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	_ = f.log.Close() // the log dies (disk full, handle gone)
	_, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1000))
	if !errors.Is(err, ErrAudit) {
		t.Fatalf("got %v, want ErrAudit", err)
	}
	if be.calls.Load() != 0 {
		t.Fatal("the HSM signed although the audit entry could not be written")
	}
	if f.svc.Ready() {
		t.Fatal("service reports ready with a dead audit log")
	}
}

func TestSign_HSMErrorsAreExplicitAndNeverFallBack(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	be.failWith = &hsm.Error{Code: hsm.CodeUnavailable, Op: "sign", Cause: errors.New("CKR_DEVICE_ERROR")}
	_, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1000))
	var he *hsm.Error
	if !errors.As(err, &he) || he.Code != hsm.CodeUnavailable {
		t.Fatalf("got %v", err)
	}
	if be.calls.Load() != 1 {
		t.Fatalf("calls=%d: no retry on another path", be.calls.Load())
	}
	es := entries(t, f.logPath)
	if len(es) != 2 || es[0].Result != audit.ResultAuthorized || es[1].Result != audit.ResultHSMError {
		t.Fatalf("entries: %+v", es)
	}
	// the failed attempt still counts in the window (conservative), and the same nonce retries fine
	be.failWith = nil
	if _, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1000)); err != nil {
		t.Fatalf("retry of the same nonce: %v", err)
	}
}

func TestSign_AWrongSignatureIsNeverReturnedAndClosesTheGateway(t *testing.T) {
	for name, mangle := range map[string]func([]byte) []byte{
		"signature by another key": nil, // filled below
		"corrupted r":              func(rs []byte) []byte { rs[3] ^= 0xff; return rs },
		"short output":             func(rs []byte) []byte { return rs[:60] },
	} {
		t.Run(name, func(t *testing.T) {
			be := newSoft(t)
			f := newFixture(t, be, 1337)
			if mangle == nil {
				other := newSoft(t)
				be.mangle = func([]byte) []byte { rs, _ := other.SignDigest(context.Background(), make([]byte, 32)); return rs }
			} else {
				be.mangle = mangle
			}
			res, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1000))
			if err == nil || res != nil {
				t.Fatalf("a bad signature was returned: %v", res)
			}
			be.mangle = nil
			if _, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 1, user, 1000)); !errors.Is(err, ErrNotServing) {
				t.Fatalf("after a bad signature the gateway must stop serving, got %v", err)
			}
		})
	}
}

func TestNew_RefusesAKeyThatIsNotThePinnedHotAddress(t *testing.T) {
	be := newSoft(t)
	pol, _ := policy.Parse([]byte(policyFor(common.HexToAddress("0x1111111111111111111111111111111111111111"), 1337)))
	l, _, _ := audit.Open(filepath.Join(t.TempDir(), "a.log"))
	defer l.Close()
	if _, err := New(pol, l, nil, be, nil); err == nil {
		t.Fatal("started with an HSM key that is not the pinned Hot address")
	}
}

// ---- self-test

func TestSelfTest_PassesFailsClosedAndLatchesOnMismatch(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	if err := f.svc.SelfTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	es := entries(t, f.logPath)
	if es[0].Op != audit.OpSelfTest || es[0].Nonce != ^uint64(0) {
		t.Fatalf("self-test is not audited as the nonce 2^64-1 transaction: %+v", es[0])
	}
	// attribute check failure (key became extractable / not private): refuse to serve
	be.checkErr = &hsm.Error{Code: hsm.CodeKeyAttrs, Op: "check key attributes", Cause: errors.New("CKA_PRIVATE is not true")}
	if err := f.svc.SelfTest(context.Background()); err == nil || f.svc.Serving() {
		t.Fatal("self-test with bad key attributes did not close signing")
	}
	if _, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, 0, user, 1)); !errors.Is(err, ErrNotServing) {
		t.Fatalf("sign while self-test failing: %v", err)
	}
	be.checkErr = nil // transient: the next pass reopens it
	if err := f.svc.SelfTest(context.Background()); err != nil || !f.svc.Serving() {
		t.Fatalf("did not recover after a clean self-test: %v", err)
	}
	// a recovery mismatch latches until restart, even if later tests would pass
	be.mangle = func(rs []byte) []byte { rs[40] ^= 1; return rs }
	if err := f.svc.SelfTest(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatalf("got %v", err)
	}
	be.mangle = nil
	_ = f.svc.SelfTest(context.Background())
	if f.svc.Serving() {
		t.Fatal("a recovery mismatch must latch the gateway closed until restart")
	}
}

// ---- policy hash and restart

func TestPolicyFileEditChangesTheHashLoggedInAuditEntries(t *testing.T) {
	be := newSoft(t)
	dir := t.TempDir()
	polPath := filepath.Join(dir, "policy.yaml")
	logPath := filepath.Join(dir, "audit.log")
	run := func(nonce uint64) (hash string) {
		pol, err := policy.LoadFile(polPath, policy.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		l, op, err := audit.Open(logPath)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		svc, err := New(pol, l, op.Entries, be, prometheus.NewRegistry())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Sign(context.Background(), "wallet-service", payout(1337, nonce, user, 1)); err != nil {
			t.Fatal(err)
		}
		es := entries(t, logPath)
		return es[len(es)-1].PolicyHash
	}
	if err := os.WriteFile(polPath, []byte(policyFor(be.Address(), 1337)), 0o644); err != nil {
		t.Fatal(err)
	}
	h1 := run(0)
	if h1b := run(1); h1b != h1 {
		t.Fatal("hash changed without an edit")
	}
	edited := strings.Replace(policyFor(be.Address(), 1337), `window_1h: "5000000000000000000"`, `window_1h: "4000000000000000000"`, 1)
	if err := os.WriteFile(polPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	h2 := run(2)
	if h1 == h2 || len(h2) != 64 {
		t.Fatalf("policy edit did not change the logged hash: %s vs %s", h1, h2)
	}
	t.Logf("policy hash before edit %.16s..., after edit %.16s...", h1, h2)
}

func TestRestart_PolicyWindowsAreRebuiltFromTheAuditLog(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	for n := uint64(0); n < 4; n++ { // 4 x (1 ETH + fee) leaves no room for a 5th in the 5 ETH 1 h window
		in := payout(1337, n, user, 1_000_000_000_000_000_000)
		if _, err := f.svc.Sign(context.Background(), "wallet-service", in); err != nil {
			t.Fatalf("payout %d: %v", n, err)
		}
	}
	_ = f.log.Close()
	// "restart": reopen the log, rebuild the service from it
	l, op, err := audit.Open(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	svc, err := New(f.pol, l, op.Entries, be, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Sign(context.Background(), "wallet-service", payout(1337, 4, user, 1_000_000_000_000_000_000))
	var pr *PolicyRefused
	if !errors.As(err, &pr) || pr.Rule != policy.RuleWindow1h {
		t.Fatalf("after restart: %v, want window_1h refusal (state was not rebuilt)", err)
	}
}

// ---- a real EVM mines what the gateway signs

func TestSign_TransactionsAreAcceptedAndMinedByARealEVM(t *testing.T) {
	be := newSoft(t)
	RunChainTest(t, be, func(t *testing.T, chainID uint64) *Service { return newFixture(t, be, chainID).svc })
}

// RunChainTest signs through svc and has go-ethereum's simulated EVM mine the results: three payouts in
// nonce order, a Fee Bump replacing a pending payout, and a Cancel replacing another. It is exported
// so the SoftHSM integration test runs the identical scenario against the real token.
func RunChainTest(t *testing.T, be hsm.Backend, mk func(t *testing.T, chainID uint64) *Service) {
	t.Helper()
	hot := be.Address()
	backend := simulated.NewBackend(types.GenesisAlloc{hot: {Balance: new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	client := backend.Client()
	ctx := context.Background()
	id, _ := client.ChainID(ctx)
	svc := mk(t, id.Uint64())
	to := common.HexToAddress(user)

	send := func(res *Result, err error) *types.Transaction {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var tx types.Transaction
		if err := tx.UnmarshalBinary(res.RawTx); err != nil {
			t.Fatal(err)
		}
		if err := client.SendTransaction(ctx, &tx); err != nil {
			t.Fatalf("the EVM node rejected the gateway's signature: %v", err)
		}
		return &tx
	}
	mustMine := func(tx *types.Transaction, wantValue int64) {
		t.Helper()
		backend.Commit()
		rcpt, err := client.TransactionReceipt(ctx, tx.Hash())
		if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
			t.Fatalf("tx not mined successfully: %v %+v", err, rcpt)
		}
		if tx.Value().Int64() != wantValue {
			t.Fatalf("value %v", tx.Value())
		}
	}

	var txs []*types.Transaction
	for n := uint64(0); n < 3; n++ {
		txs = append(txs, send(svc.Sign(ctx, "wallet-service", payout(id.Uint64(), n, user, 1_000_000_000_000_000))))
	}
	backend.Commit()
	for _, tx := range txs {
		if rcpt, err := client.TransactionReceipt(ctx, tx.Hash()); err != nil || rcpt.Status != 1 {
			t.Fatalf("payout nonce %d not mined: %v", tx.Nonce(), err)
		}
	}
	if bal, _ := client.BalanceAt(ctx, to, nil); bal.Int64() != 3_000_000_000_000_000 {
		t.Fatalf("recipient balance %v", bal)
	}

	// Fee Bump: sign nonce 3, send it (pending), sign the bump, send it: the node replaces it, and the bump mines.
	first := send(svc.Sign(ctx, "wallet-service", payout(id.Uint64(), 3, user, 7)))
	bump := payout(id.Uint64(), 3, user, 7)
	bump.MaxFee, bump.Tip = big.NewInt(20_000_000_000), big.NewInt(2_000_000_000)
	bumped := send(svc.Sign(ctx, "wallet-service", bump))
	if first.Hash() == bumped.Hash() {
		t.Fatal("bump is the same tx")
	}
	mustMine(bumped, 7)

	// Cancel: sign nonce 4 as a payout (pending), then a Cancel for the same nonce at a higher fee.
	_ = send(svc.Sign(ctx, "wallet-service", payout(id.Uint64(), 4, user, 9)))
	c := &intent.Intent{ChainID: id.Uint64(), Nonce: 4, To: hot, Value: new(big.Int), Gas: 21000, MaxFee: big.NewInt(20_000_000_000),
		Tip: big.NewInt(2_000_000_000), Operation: intent.OpCancel, IdempotencyKey: "cancel4"}
	cancelled := send(svc.Sign(ctx, "wallet-service", c))
	mustMine(cancelled, 0)
	if bal, _ := client.BalanceAt(ctx, to, nil); bal.Int64() != 3_000_000_000_000_007 {
		t.Fatalf("recipient balance %v: the cancelled payout must not have been paid", bal)
	}
}

// ---- crash safety: kill the gateway mid-sign

// The helper process: a gateway over a software backend that reads Sign Intents (JSON lines) on stdin and prints one
// result line per intent. HSMGW_KILL_AT=k kills the process with SIGKILL inside the k-th SignDigest call, i.e. after the
// audit entry was fsynced and before any signature could be returned.
func TestMain(m *testing.M) {
	switch os.Getenv("HSMGW_HELPER") {
	case "1":
		helperMain()
		return
	case "open":
		openHelperMain() // softhsm-tagged build only
		return
	}
	os.Exit(m.Run())
}

type helperLine struct {
	ID  int    `json:"id"`
	Raw string `json:"raw,omitempty"`
	Err string `json:"err,omitempty"`
}

// killBackend SIGKILLs the process inside the n-th SignDigest call (n > 0).
type killBackend struct {
	hsm.Backend
	n     int
	calls atomic.Int64
}

func (k *killBackend) SignDigest(ctx context.Context, d []byte) ([]byte, error) {
	if k.n > 0 && int(k.calls.Add(1)) == k.n {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // never returns a signature
	}
	return k.Backend.SignDigest(ctx, d)
}

func helperMain() {
	var inner hsm.Backend
	if os.Getenv("HSMGW_SOFTHSM") == "1" {
		inner = openRealToken() // softhsm-tagged build only
	} else {
		keyBytes, _ := hex.DecodeString(os.Getenv("HSMGW_KEY"))
		key, _ := crypto.ToECDSA(keyBytes)
		inner = &softBackend{key: key}
	}
	var n int
	_, _ = fmt.Sscanf(os.Getenv("HSMGW_KILL_AT"), "%d", &n)
	be := &killBackend{Backend: inner, n: n}
	pol, err := policy.Parse([]byte(policyFor(be.Address(), 1337)))
	if err != nil {
		panic(err)
	}
	l, op, err := audit.Open(os.Getenv("HSMGW_AUDIT"))
	if err != nil {
		panic(err)
	}
	svc, err := New(pol, l, op.Entries, be, nil)
	if err != nil {
		panic(err)
	}
	var out sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	var wg sync.WaitGroup
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID    int    `json:"id"`
			Nonce uint64 `json:"nonce"`
			Value int64  `json:"value"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Sign(context.Background(), "wallet-service", payout(1337, req.Nonce, user, req.Value))
			line := helperLine{ID: req.ID}
			if err != nil {
				line.Err = err.Error()
			} else {
				line.Raw = hex.EncodeToString(res.RawTx)
			}
			out.Lock()
			_ = enc.Encode(line)
			out.Unlock()
		}()
	}
	wg.Wait()
}

type helper struct {
	cmd  *exec.Cmd
	in   *json.Encoder
	inW  interface{ Close() error }
	out  *bufio.Scanner
	sigs map[int]string
	errs map[int]string
}

func startHelper(t *testing.T, keyHex, auditPath string, killAt int, extraEnv ...string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HSMGW_HELPER=1", "HSMGW_KEY="+keyHex, "HSMGW_AUDIT="+auditPath, fmt.Sprintf("HSMGW_KILL_AT=%d", killAt))
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &helper{cmd: cmd, in: json.NewEncoder(in), inW: in, out: bufio.NewScanner(out), sigs: map[int]string{}, errs: map[int]string{}}
}

func (h *helper) collect() {
	for h.out.Scan() {
		var l helperLine
		if json.Unmarshal(h.out.Bytes(), &l) == nil {
			if l.Raw != "" {
				h.sigs[l.ID] = l.Raw
			} else {
				h.errs[l.ID] = l.Err
			}
		}
	}
	_ = h.cmd.Wait()
}

func digestOfRaw(t *testing.T, rawHex string) string {
	t.Helper()
	raw, _ := hex.DecodeString(rawHex)
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	d := types.LatestSignerForChainID(big.NewInt(1337)).Hash(&tx)
	return "0x" + hex.EncodeToString(d[:])
}

// assertNoSignatureWithoutAuditLine: every signature a client received has an authorized audit entry, and the chain verifies.
func assertNoSignatureWithoutAuditLine(t *testing.T, logPath string, sigs map[int]string) (authorized int) {
	t.Helper()
	if _, _, err := audit.Verify(logPath, audit.VerifyOptions{}); err != nil {
		// a torn final line is the expected result of SIGKILL mid-append; Open trims it
		l, _, oerr := audit.Open(logPath)
		if oerr != nil {
			t.Fatalf("audit log corrupt after a crash: %v", oerr)
		}
		_ = l.Close()
	}
	have := map[string]bool{}
	for _, e := range entries(t, logPath) {
		if e.Op == audit.OpHotSign && e.Result == audit.ResultAuthorized {
			have[e.Digest] = true
			authorized++
		}
	}
	for id, raw := range sigs {
		if !have[digestOfRaw(t, raw)] {
			t.Fatalf("request %d: a signature was returned with no authorizing audit line", id)
		}
	}
	return authorized
}

func TestCrash_KillInsideSignReturnsNoSignatureButLeavesTheAuditLine(t *testing.T) {
	key := newSoft(t)
	keyHex := hex.EncodeToString(crypto.FromECDSA(key.key))
	logPath := filepath.Join(t.TempDir(), "audit.log")

	h := startHelper(t, keyHex, logPath, 3) // dies inside the 3rd HSM call
	for i := 0; i < 5; i++ {
		_ = h.in.Encode(map[string]any{"id": i, "nonce": i, "value": 1})
		time.Sleep(150 * time.Millisecond) // sequential: let each finish
	}
	_ = h.inW.Close()
	h.collect()
	if len(h.sigs) != 2 {
		t.Fatalf("got %d signatures (ids %v), want exactly the 2 before the kill", len(h.sigs), h.sigs)
	}
	if _, ok := h.sigs[2]; ok {
		t.Fatal("request 2 was killed inside the HSM call yet a signature came back")
	}
	authorized := assertNoSignatureWithoutAuditLine(t, logPath, h.sigs)
	if authorized != 3 {
		t.Fatalf("%d authorized audit entries, want 3 (the killed request's line is the evidence it was in flight)", authorized)
	}
	t.Logf("signatures returned: %d, authorized audit lines: %d; the killed request has a line and no signature", len(h.sigs), authorized)

	// restart on the same log: windows include the lost request, and the same nonce signs again
	h2 := startHelper(t, keyHex, logPath, 0)
	_ = h2.in.Encode(map[string]any{"id": 2, "nonce": 2, "value": 1})
	_ = h2.inW.Close()
	h2.collect()
	if _, ok := h2.sigs[2]; !ok {
		t.Fatalf("retry of the killed nonce failed after restart: %v", h2.errs)
	}
	assertNoSignatureWithoutAuditLine(t, logPath, h2.sigs)
}

func TestCrash_RandomSIGKILLsUnderLoadNeverReturnASignatureWithoutAnAuditLine(t *testing.T) {
	key := newSoft(t)
	keyHex := hex.EncodeToString(crypto.FromECDSA(key.key))
	logPath := filepath.Join(t.TempDir(), "audit.log")
	totalSigs, totalAuth := 0, 0
	for round := 0; round < 6; round++ {
		h := startHelper(t, keyHex, logPath, 0)
		go func() {
			time.Sleep(time.Duration(40+round*17) * time.Millisecond)
			_ = h.cmd.Process.Kill() // SIGKILL at an arbitrary point
		}()
		go func() {
			for i := 0; i < 400; i++ {
				if err := h.in.Encode(map[string]any{"id": i, "nonce": round*1000 + i, "value": 1}); err != nil {
					return
				}
			}
			_ = h.inW.Close()
		}()
		h.collect()
		totalSigs += len(h.sigs)
		totalAuth = assertNoSignatureWithoutAuditLine(t, logPath, h.sigs)
	}
	if totalSigs == 0 {
		t.Fatal("no signature was ever returned: the test did not exercise the path")
	}
	t.Logf("6 SIGKILL rounds: %d signatures returned, %d authorized audit lines in the final log, every signature has its line", totalSigs, totalAuth)
}
