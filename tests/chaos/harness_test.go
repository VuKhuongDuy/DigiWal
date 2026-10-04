// Package chaos drives the REAL stack (all 7 services, tests/e2e/stack2.sh) only through api-gateway's public
// routes, anvil JSON-RPC and docker. It breaks things on purpose (kill -9, pause, restart, reorg) and then checks
// that money is neither lost nor duplicated, on chain AND in Postgres. Run: CHAOS=1 go test -v -p 1 ./...
package chaos

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

//go:embed contracts/MockERC20.bin
var mockERC20Bin string

const (
	rpcURL     = "http://localhost:18545"
	gatewayURL = "http://localhost:18084"
	walletTLS  = "https://localhost:18443"
	ethAsset   = "11111111-1111-1111-1111-111111111111"
	ttAsset    = "22222222-2222-2222-2222-222222222222"
	tokenAddr  = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	key0Hex    = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	key1Hex    = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	project    = "digiwal-qa"
)

var (
	oneETH   = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	chainID  = big.NewInt(31337)
	ec       *ethclient.Client
	rc       *rpc.Client
	key0     *ecdsa.PrivateKey
	key1     *ecdsa.PrivateKey
	hot      common.Address
	token    = common.HexToAddress(tokenAddr)
	repoRoot string
	fundMu   sync.Mutex
)

func TestMain(m *testing.M) {
	if os.Getenv("CHAOS") == "" {
		fmt.Println("CHAOS not set — skipping (needs tests/e2e/stack2.sh up)")
		os.Exit(0)
	}
	wd, _ := os.Getwd()
	repoRoot = strings.TrimSuffix(wd, "/tests/chaos")
	ctx := context.Background()
	var err error
	die := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "setup:", e)
			os.Exit(2)
		}
	}
	rc, err = rpc.DialContext(ctx, rpcURL)
	die(err)
	ec = ethclient.NewClient(rc)
	key0, _ = crypto.HexToECDSA(key0Hex)
	key1, _ = crypto.HexToECDSA(key1Hex)
	hot, err = hotAddress(ctx)
	die(err)
	// token at the registered address: deploy first thing on a fresh chain
	if code, _ := ec.CodeAt(ctx, token, nil); len(code) == 0 {
		raw, _ := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(mockERC20Bin), "0x"))
		u256, _ := abi.NewType("uint256", "", nil)
		arg, _ := abi.Arguments{{Type: u256}}.Pack(new(big.Int).Mul(big.NewInt(1_000_000), oneETH))
		r, e := sendRaw(ctx, key0, nil, big.NewInt(0), append(raw, arg...))
		die(e)
		if r.ContractAddress != token {
			die(fmt.Errorf("token deployed at %s, registry expects %s: stack must be fresh (stack2.sh down up)", r.ContractAddress, token))
		}
	}
	if b, _ := ec.BalanceAt(ctx, hot, nil); b.Cmp(new(big.Int).Mul(big.NewInt(100), oneETH)) < 0 {
		_, e := sendRaw(ctx, key1, &hot, new(big.Int).Mul(big.NewInt(200), oneETH), nil)
		die(e)
	}
	if tokenBal(hot).Sign() == 0 {
		_, e := sendRaw(ctx, key0, &token, big.NewInt(0), transferData(hot, new(big.Int).Mul(big.NewInt(100000), oneETH)))
		die(e)
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------- chain

func sendRaw(ctx context.Context, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) (*types.Receipt, error) {
	fundMu.Lock()
	defer fundMu.Unlock()
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := ec.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	head, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tip := big.NewInt(1e9)
	gas := uint64(3_000_000)
	if to != nil {
		if gas, err = ec.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data}); err != nil {
			return nil, fmt.Errorf("estimate: %w", err)
		}
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip), Gas: gas, To: to, Value: value, Data: data})
	if err != nil {
		return nil, err
	}
	if err := ec.SendTransaction(ctx, tx); err != nil {
		return nil, err
	}
	for i := 0; i < 150; i++ {
		if r, err := ec.TransactionReceipt(ctx, tx.Hash()); err == nil {
			return r, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("no receipt for %s", tx.Hash())
}

func transferData(to common.Address, v *big.Int) []byte {
	return append(crypto.Keccak256([]byte("transfer(address,uint256)"))[:4], append(common.LeftPadBytes(to.Bytes(), 32), common.LeftPadBytes(v.Bytes(), 32)...)...)
}

func tokenBal(who common.Address) *big.Int {
	out, err := ec.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(who.Bytes(), 32)...)}, nil)
	if err != nil {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(out)
}

func ethBal(who common.Address) *big.Int {
	b, err := ec.BalanceAt(context.Background(), who, nil)
	if err != nil {
		return new(big.Int)
	}
	return b
}

func mine(t testing.TB, n int) {
	t.Helper()
	if err := rc.CallContext(context.Background(), nil, "anvil_mine", fmt.Sprintf("0x%x", n)); err != nil {
		t.Fatalf("anvil_mine: %v", err)
	}
}

func rpcCall(t testing.TB, out any, method string, params ...any) {
	t.Helper()
	if err := rc.CallContext(context.Background(), out, method, params...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func fresh() common.Address { k, _ := crypto.GenerateKey(); return crypto.PubkeyToAddress(k.PublicKey) }

func finney(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }
func eth1(n int64) *big.Int   { return new(big.Int).Mul(big.NewInt(n), oneETH) }

// ---------------------------------------------------------------- gateway user

type user struct {
	id, key string
	dep     common.Address
}

func do(method, path, key string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, gatewayURL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func newUser(t testing.TB) *user {
	t.Helper()
	code, b := do("POST", "/users", "", map[string]string{})
	var x struct {
		UserID string `json:"user_id"`
		APIKey string `json:"api_key"`
	}
	if code != 201 || json.Unmarshal(b, &x) != nil {
		t.Fatalf("POST /users: %d %s", code, b)
	}
	u := &user{id: x.UserID, key: x.APIKey}
	code, b = do("GET", "/v1/deposit-address", u.key, nil)
	var a struct{ Address string }
	if code/100 != 2 || json.Unmarshal(b, &a) != nil {
		t.Fatalf("deposit-address: %d %s", code, b)
	}
	u.dep = common.HexToAddress(a.Address)
	return u
}

type bal struct{ Total, Locked, Avail *big.Int }

func (u *user) bal(t testing.TB, asset string) bal {
	t.Helper()
	code, b := do("GET", "/v1/balances", u.key, nil)
	var rs []map[string]string
	if code != 200 || json.Unmarshal(b, &rs) != nil {
		t.Fatalf("balances: %d %s", code, b)
	}
	for _, r := range rs {
		if r["asset_id"] == asset {
			a, _ := new(big.Int).SetString(r["total_balance"], 10)
			l, _ := new(big.Int).SetString(r["locked_balance"], 10)
			v, _ := new(big.Int).SetString(r["available_balance"], 10)
			return bal{a, l, v}
		}
	}
	return bal{new(big.Int), new(big.Int), new(big.Int)}
}

// fundAndCredit deposits `amount` of asset to the user's address on chain and waits until the ledger shows it
// (chain-watcher -> Kafka -> ledger). Native only unless asset is TT.
func (u *user) fundAndCredit(t testing.TB, asset string, amount *big.Int) {
	t.Helper()
	before := u.bal(t, asset).Total
	ctx := context.Background()
	if asset == ethAsset {
		if _, err := sendRaw(ctx, key1, &u.dep, amount, nil); err != nil {
			t.Fatal(err)
		}
	} else if _, err := sendRaw(ctx, key0, &token, big.NewInt(0), transferData(u.dep, amount)); err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Add(before, amount)
	waitFor(t, 90*time.Second, "deposit credited", func() (bool, string) {
		mine(t, 1)
		got := u.bal(t, asset).Total
		return got.Cmp(want) == 0, "have " + got.String() + " want " + want.String()
	})
}

func (u *user) whitelist(t testing.TB, dest common.Address) {
	t.Helper()
	if code, b := do("POST", "/v1/whitelist", u.key, map[string]string{"address": dest.Hex()}); code != 201 {
		t.Fatalf("whitelist %s: %d %s", dest, code, b)
	}
	waitFor(t, 60*time.Second, "whitelist active", func() (bool, string) {
		_, b := do("GET", "/v1/whitelist", u.key, nil)
		var rows []struct{ Address, Status string }
		json.Unmarshal(b, &rows)
		for _, r := range rows {
			if strings.EqualFold(r.Address, dest.Hex()) {
				return r.Status == "active", r.Status
			}
		}
		return false, "absent"
	})
}

func (u *user) withdraw(key, asset string, amount *big.Int, to common.Address) (int, []byte) {
	return do("POST", "/v1/withdrawals", u.key, map[string]string{"idempotency_key": key, "asset_id": asset, "amount": amount.String(), "to_address": to.Hex()})
}

func (u *user) wstatus(key string) (string, string) {
	code, b := do("GET", "/v1/withdrawals/"+key, u.key, nil)
	if code != 200 {
		return fmt.Sprintf("http-%d", code), ""
	}
	var r struct {
		Status string  `json:"status"`
		TxHash *string `json:"tx_hash"`
	}
	json.Unmarshal(b, &r)
	h := ""
	if r.TxHash != nil {
		h = *r.TxHash
	}
	return r.Status, h
}

func terminal(s string) bool { return s == "confirmed" || s == "rejected" || s == "failed" || s == "disputed" }

// settle drives time forward (blocks) until every key is terminal or the deadline passes. Returns key->status.
func (u *user) settleAll(t testing.TB, keys []string, d time.Duration) map[string]string {
	t.Helper()
	out := map[string]string{}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		done := true
		for _, k := range keys {
			s, _ := u.wstatus(k)
			out[k] = s
			if !terminal(s) {
				done = false
			}
		}
		if done {
			return out
		}
		mine(t, 1)
		time.Sleep(700 * time.Millisecond)
	}
	return out
}

func waitFor(t testing.TB, d time.Duration, what string, ok func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		var done bool
		if done, last = ok(); done {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s; last: %s", d, what, last)
}

// ---------------------------------------------------------------- docker / psql / mTLS

func dc(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Logf("docker %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func ctr(svc string) string { return project + "-" + svc + "-1" }

func psql(t testing.TB, db, sql string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", ctr("postgres"), "psql", "-U", "custody", "-d", db, "-tA", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %s: %v %s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hotAddress(ctx context.Context) (common.Address, error) {
	dir := repoRoot + "/infra/pki/out/sweeper"
	if repoRoot == "" {
		wd, _ := os.Getwd()
		dir = strings.TrimSuffix(wd, "/tests/chaos") + "/infra/pki/out/sweeper"
	}
	cert, err := tls.LoadX509KeyPair(dir+"/cert.pem", dir+"/key.pem")
	if err != nil {
		return common.Address{}, err
	}
	ca, _ := os.ReadFile(dir + "/ca-cert.pem")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "wallet-service", MinVersion: tls.VersionTLS13}}}
	req, _ := http.NewRequestWithContext(ctx, "GET", walletTLS+"/v1/wallet/hot-address", nil)
	resp, err := c.Do(req)
	if err != nil {
		return common.Address{}, err
	}
	defer resp.Body.Close()
	var r struct{ Address string }
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &r) != nil || r.Address == "" {
		return common.Address{}, fmt.Errorf("hot-address: %d %s", resp.StatusCode, b)
	}
	return common.HexToAddress(r.Address), nil
}

func ctxBG() context.Context { return context.Background() }
