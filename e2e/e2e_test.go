// Task 2.6: deposit -> chain-watcher -> Kafka -> ledger -> api-gateway balance, plus the
// sweeper moving the funds to the Hot Wallet, against the stack `make up` starts (real
// Anvil chain, every M2 service as its shipped container). Skipped unless E2E=1 so a plain
// `go test ./...` at the repo root never needs Docker.
package e2e

import (
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
	"path/filepath"
	"strings"
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
	rpcURL     = "http://localhost:8545"
	gatewayURL = "http://localhost:8084"
	walletTLS  = "https://localhost:8443"

	ethAssetID   = "11111111-1111-1111-1111-111111111111"
	tokenAssetID = "22222222-2222-2222-2222-222222222222"
	// The address Anvil assigns the first contract deployed by account #0 (nonce 0);
	// e2e/assets.yaml registers the test token here.
	tokenAddress = "0x5FbDB2315678afecb367f032d93F642f64180aa3"

	// Anvil's well-known default keys (public, dev-only): #0 deploys the token and holds
	// its supply, #1 pays the ETH deposits and funds the Hot Wallet.
	anvilKey0 = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilKey1 = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
)

var (
	oneETH  = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	chainID = big.NewInt(31337)
)

func TestDepositCreditsBalanceAndSweepsToHotWallet(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 (make e2e) to run against the docker-compose stack")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	rpcc, _ := rpc.DialContext(ctx, rpcURL)
	key0, key1 := mustKey(t, anvilKey0), mustKey(t, anvilKey1)

	// Token first: its address is fixed only while account #0 has sent nothing else.
	token := deployToken(ctx, t, eth, key0)
	hot := hotWalletAddress(ctx, t)
	send(ctx, t, eth, key1, &hot, new(big.Int).Mul(big.NewInt(10), oneETH), nil) // gas float for top-ups
	hotETH0 := balance(ctx, t, eth, hot)
	hotTT0 := tokenBalance(ctx, t, eth, token, hot)

	// Alice deposits only ETH and bob only the token: a token sweep needs a Gas Top-Up (native
	// ETH from the Hot Wallet), and keeping the two apart keeps each user's expectations exact.
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	aliceAddr := common.HexToAddress(alice.depositAddress(t))
	bobAddr := common.HexToAddress(bob.depositAddress(t))
	if again := alice.depositAddress(t); !strings.EqualFold(again, aliceAddr.Hex()) {
		t.Fatalf("deposit address is not stable: %s then %s", aliceAddr.Hex(), again)
	}
	if aliceAddr == bobAddr {
		t.Fatal("two users share a deposit address")
	}

	ethAmt := new(big.Int).Mul(big.NewInt(2), oneETH)
	ttAmt := new(big.Int).Mul(big.NewInt(500), oneETH)
	send(ctx, t, eth, key1, &aliceAddr, ethAmt, nil)
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, bobAddr, ttAmt))
	mine(ctx, t, rpcc, 5) // Confirmation Depth is 2 in this stack

	// Balance: chain-watcher -> deposits.confirmed -> ledger -> gateway (mTLS).
	t.Run("native deposit is credited", func(t *testing.T) {
		waitFor(t, 90*time.Second, "alice's ETH balance", func() (bool, string) {
			b := alice.balances(t)
			return b[ethAssetID] == ethAmt.String(), fmt.Sprint(b)
		})
	})
	t.Run("erc20 deposit is credited", func(t *testing.T) {
		waitFor(t, 90*time.Second, "bob's TT balance", func() (bool, string) {
			b := bob.balances(t)
			return b[tokenAssetID] == ttAmt.String(), fmt.Sprint(b)
		})
	})

	// Sweep: both deposit addresses are emptied into the Hot Wallet.
	t.Run("deposits are swept to the hot wallet", func(t *testing.T) {
		waitFor(t, 120*time.Second, "sweeps", func() (bool, string) {
			mine(ctx, t, rpcc, 1)
			tt := tokenBalance(ctx, t, eth, token, bobAddr)
			ethLeft := balance(ctx, t, eth, aliceAddr)
			swept := tt.Sign() == 0 && ethLeft.Cmp(big.NewInt(1e15)) < 0
			return swept, fmt.Sprintf("bob TT on chain %s, alice ETH on chain %s", tt, ethLeft)
		})
		if got, want := tokenBalance(ctx, t, eth, token, hot), new(big.Int).Add(hotTT0, ttAmt); got.Cmp(want) != 0 {
			t.Errorf("Hot Wallet TT = %s, want %s", got, want)
		}
		// The 2 ETH swept in dwarfs the gas the Hot Wallet spent on top-ups and its own fees.
		if got := balance(ctx, t, eth, hot); got.Cmp(hotETH0) <= 0 {
			t.Errorf("Hot Wallet ETH did not grow: %s -> %s", hotETH0, got)
		}
	})

	// Isolation and no double credit: carol saw nothing, and neither balance moved again.
	t.Run("no stray credits", func(t *testing.T) {
		time.Sleep(5 * time.Second)
		if b := carol.balances(t); b[ethAssetID] != "0" || b[tokenAssetID] != "0" {
			t.Errorf("carol was credited: %v", b)
		}
		if b := alice.balances(t); b[ethAssetID] != ethAmt.String() || b[tokenAssetID] != "0" {
			t.Errorf("alice's balance is wrong after the sweep: %v", b)
		}
		if b := bob.balances(t); b[tokenAssetID] != ttAmt.String() {
			t.Errorf("bob's TT balance moved after the sweep: %v", b)
		}
	})

	// Regression for the bug this suite found (deposit-flow.md invariant #17): the sweeper's Gas
	// Top-Up (0.001 ETH, Hot Wallet -> bob's address) is a native transfer to a watched address, and
	// used to be reported as a deposit, crediting bob ETH he never sent. Mine well past Confirmation
	// Depth first, so a wrongly-recorded top-up would already be confirmed and credited.
	t.Run("gas top-up is not credited as a deposit", func(t *testing.T) {
		mine(ctx, t, rpcc, 5)
		time.Sleep(5 * time.Second)
		if b := bob.balances(t); b[ethAssetID] != "0" {
			t.Errorf("bob's ETH balance is %s, want 0: the Hot Wallet's Gas Top-Up was credited as his deposit", b[ethAssetID])
		}
	})
}

// ---- users / gateway ------------------------------------------------------------------

type user struct {
	id, key string
}

func newUser(t *testing.T) *user {
	t.Helper()
	resp, err := http.Post(gatewayURL+"/users", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var u struct {
		UserID string `json:"user_id"`
		APIKey string `json:"api_key"`
	}
	decode(t, resp, http.StatusCreated, &u)
	return &user{id: u.UserID, key: u.APIKey}
}

func (u *user) get(t *testing.T, path string, out any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, gatewayURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+u.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The first deposit-address call creates the address and passes wallet-service's 201 through.
	decode(t, resp, 0, out)
}

func (u *user) depositAddress(t *testing.T) string {
	var r struct{ Address string }
	u.get(t, "/v1/deposit-address", &r)
	return r.Address
}

// balances maps asset_id -> total_balance.
func (u *user) balances(t *testing.T) map[string]string {
	var rows []struct {
		AssetID      string `json:"asset_id"`
		TotalBalance string `json:"total_balance"`
	}
	u.get(t, "/v1/balances", &rows)
	m := map[string]string{}
	for _, r := range rows {
		m[r.AssetID] = r.TotalBalance
	}
	return m
}

func decode(t *testing.T, resp *http.Response, want int, out any) {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want && !(want == 0 && resp.StatusCode/100 == 2) { // want 0 = any 2xx
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

// hotWalletAddress asks wallet-service over mTLS. The endpoint admits only CN=sweeper, so the
// test borrows the sweeper's leaf certificate (gitignored, from `make certs`).
func hotWalletAddress(ctx context.Context, t *testing.T) common.Address {
	t.Helper()
	dir := filepath.Join("..", "infra", "pki", "out", "sweeper")
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "wallet-service", MinVersion: tls.VersionTLS13}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, walletTLS+"/v1/wallet/hot-address", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ Address string }
	decode(t, resp, http.StatusOK, &r)
	return common.HexToAddress(r.Address)
}

// ---- chain helpers --------------------------------------------------------------------

func deployToken(ctx context.Context, t *testing.T, eth *ethclient.Client, key *ecdsa.PrivateKey) common.Address {
	t.Helper()
	code, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(mockERC20Bin), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	uint256, _ := abi.NewType("uint256", "", nil)
	arg, _ := abi.Arguments{{Type: uint256}}.Pack(new(big.Int).Mul(big.NewInt(1_000_000), oneETH))
	rc := send(ctx, t, eth, key, nil, big.NewInt(0), append(code, arg...))
	if rc.ContractAddress != common.HexToAddress(tokenAddress) {
		t.Fatalf("token deployed at %s, but e2e/assets.yaml registers %s — account #0 must send nothing before the test (fresh chain: make down && make up)", rc.ContractAddress, tokenAddress)
	}
	return rc.ContractAddress
}

func transferCalldata(t *testing.T, to common.Address, amount *big.Int) []byte {
	t.Helper()
	addr, _ := abi.NewType("address", "", nil)
	uint256, _ := abi.NewType("uint256", "", nil)
	args, err := abi.Arguments{{Type: addr}, {Type: uint256}}.Pack(to, amount)
	if err != nil {
		t.Fatal(err)
	}
	return append(crypto.Keccak256([]byte("transfer(address,uint256)"))[:4], args...)
}

func tokenBalance(ctx context.Context, t *testing.T, eth *ethclient.Client, token, who common.Address) *big.Int {
	t.Helper()
	data := append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(who.Bytes(), 32)...)
	out, err := eth.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return new(big.Int).SetBytes(out)
}

func balance(ctx context.Context, t *testing.T, eth *ethclient.Client, who common.Address) *big.Int {
	t.Helper()
	b, err := eth.BalanceAt(ctx, who, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// send signs and sends one tx (Anvil automines it) and returns its receipt.
func send(ctx context.Context, t *testing.T, eth *ethclient.Client, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	t.Helper()
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := eth.PendingNonceAt(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	tip := big.NewInt(1e9)
	head, err := eth.HeaderByNumber(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	gas := uint64(3_000_000)
	if to != nil {
		if gas, err = eth.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data}); err != nil {
			t.Fatalf("estimating gas: %v", err)
		}
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap, Gas: gas, To: to, Value: value, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eth.SendTransaction(ctx, tx); err != nil {
		t.Fatalf("sending tx: %v", err)
	}
	for {
		rc, err := eth.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			if rc.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("tx %s reverted", tx.Hash())
			}
			return rc
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for receipt: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func mine(ctx context.Context, t *testing.T, c *rpc.Client, n int) {
	t.Helper()
	if err := c.CallContext(ctx, nil, "anvil_mine", fmt.Sprintf("0x%x", n)); err != nil {
		t.Fatalf("anvil_mine: %v", err)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() (bool, string)) {
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
	t.Fatalf("timed out after %s waiting for %s; last seen: %s", d, what, last)
}

func mustKey(t *testing.T, hexKey string) *ecdsa.PrivateKey {
	k, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
