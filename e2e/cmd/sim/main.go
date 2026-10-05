// sim drives the local e2e stack (`make up`) by hand: create users, list them with their balances,
// deposit on-chain into a user's deposit address, and withdraw through api-gateway. It talks only to
// what a real client could: api-gateway's public routes and Anvil's JSON-RPC (plus wallet-service's
// mTLS hot-address endpoint, once, to float the Hot Wallet). Dev tool, local Anvil only.
//
//	go run ./cmd/sim init
//	go run ./cmd/sim user create alice
//	go run ./cmd/sim deposit alice eth 2
//	go run ./cmd/sim withdraw alice eth 0.5 [--to 0x...]
//	go run ./cmd/sim user list
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"digiwal/e2e/contracts"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	ethAssetID   = "11111111-1111-1111-1111-111111111111"
	tokenAssetID = "22222222-2222-2222-2222-222222222222"
	// Where e2e/assets.yaml registers the test token: the first contract account #0 deploys on a fresh chain.
	tokenAddress = "0x5FbDB2315678afecb367f032d93F642f64180aa3"

	// Anvil's public default keys: #0 deploys the token and holds its supply, #1 pays ETH.
	anvilKey0 = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilKey1 = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

	confirmationDepth = 2 // the stack's CONFIRMATION_DEPTH / WITHDRAWAL_CONFIRMATION_DEPTH
)

var (
	rpcURL     = env("SIM_RPC_URL", "http://localhost:8545")
	gatewayURL = env("SIM_GATEWAY_URL", "http://localhost:8084")
	walletTLS  = env("SIM_WALLET_TLS_URL", "https://localhost:8443")
	chainID    = big.NewInt(31337)
	oneETH     = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type asset struct{ id, symbol string }

var assets = map[string]asset{
	"eth": {ethAssetID, "ETH"},
	"tt":  {tokenAssetID, "TT"},
}

func lookupAsset(s string) (asset, error) {
	if a, ok := assets[strings.ToLower(s)]; ok {
		return a, nil
	}
	return asset{}, fmt.Errorf("unknown asset %q (use eth or tt)", s)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `usage: sim <command>

  init                                   deploy the test token, float the Hot Wallet (idempotent; other commands do it on demand)
  user create [name] [-n COUNT]          register users with api-gateway (keys are saved in ` + "~/.digiwal-sim/state.json" + `)
  user list                              users, deposit addresses and live balances
  deposit <user> <eth|tt> <amount> [--no-wait]
                                         send on-chain to the user's deposit address, mine past the Confirmation Depth, wait for the credit
  withdraw <user> <eth|tt> <amount> [--to 0x..] [--no-wait]
                                         whitelist the destination if needed, request the withdrawal, mine until it is confirmed
  run [--users N] [--rate PER_MIN]       daemon: simulate N users acting at random, forever (what the dev stack's simulator service runs)
  mine [n]                               mine n blocks (default 5); the sweeper and chain-watcher only move when blocks do
  reset                                  forget saved users (after 'make down up' the stack no longer knows them)

amounts are decimal, 18 decimals for both assets (e.g. 0.5). env: SIM_RPC_URL, SIM_GATEWAY_URL, SIM_WALLET_TLS_URL, SIM_STATE_FILE.`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	switch args[0] {
	case "init":
		c, err := dial(ctx)
		if err != nil {
			return err
		}
		return c.ensureReady(ctx, true)
	case "user":
		if len(args) < 2 {
			return errors.New("user create|list")
		}
		switch args[1] {
		case "create":
			return cmdUserCreate(ctx, args[2:])
		case "list":
			return cmdUserList(ctx)
		}
		return fmt.Errorf("unknown user command %q", args[1])
	case "deposit":
		return cmdDeposit(ctx, args[1:])
	case "withdraw":
		return cmdWithdraw(ctx, args[1:])
	case "mine":
		n := 5
		if len(args) > 1 {
			if _, err := fmt.Sscan(args[1], &n); err != nil {
				return fmt.Errorf("bad block count %q", args[1])
			}
		}
		c, err := dial(ctx)
		if err != nil {
			return err
		}
		return c.mine(ctx, n)
	case "run":
		return cmdRun(args[1:])
	case "reset":
		if err := os.Remove(stateFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("saved users forgotten")
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}

// ---- saved users ----------------------------------------------------------------------

type user struct {
	Name    string `json:"name"`
	ID      string `json:"user_id"`
	APIKey  string `json:"api_key"`
	Deposit string `json:"deposit_address,omitempty"`

	// Used by `sim run` only.
	Profile  string              `json:"profile,omitempty"`      // whale | regular | lurker
	Dests    []string            `json:"destinations,omitempty"` // whitelisted external addresses
	Expected map[string]string   `json:"expected,omitempty"`     // asset id -> deposits sent minus withdrawals confirmed
	Pending  []pendingWithdrawal `json:"pending,omitempty"`      // accepted, not yet confirmed
	LastOp   time.Time           `json:"last_op,omitempty"`
	busy     bool                // a withdrawal is in flight: never overlap two (they would just lock each other out)
}

type pendingWithdrawal struct {
	Key    string    `json:"key"`
	Asset  string    `json:"asset_id"`
	Amount string    `json:"amount"`
	Since  time.Time `json:"since"`
	To     string    `json:"to"`
}

type state struct {
	Users []*user `json:"users"`
	mu    sync.Mutex
}

func stateFile() string {
	if f := os.Getenv("SIM_STATE_FILE"); f != "" {
		return f
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".digiwal-sim", "state.json")
}

func loadState() (*state, error) {
	b, err := os.ReadFile(stateFile())
	if os.IsNotExist(err) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	return &s, json.Unmarshal(b, &s)
}

// save persists the state. Callers that share the state between goroutines mutate it with `mutate`.
func (s *state) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *state) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(stateFile()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(stateFile(), b, 0o600) // holds API keys
}

func (s *state) find(name string) (*user, error) {
	for _, u := range s.Users {
		if u.Name == name {
			return u, nil
		}
	}
	return nil, fmt.Errorf("no saved user %q (see `sim user list`)", name)
}

// ---- gateway --------------------------------------------------------------------------

func (u *user) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, gatewayURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

func (u *user) mustDo(ctx context.Context, method, path string, body any, want ...int) ([]byte, error) {
	code, out, err := u.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	for _, w := range want {
		if code == w {
			return out, nil
		}
	}
	if code == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s %s: 401 — the stack does not know this user (was it reset? run `sim reset`)", method, path)
	}
	return nil, fmt.Errorf("%s %s: %d %s", method, path, code, strings.TrimSpace(string(out)))
}

func (u *user) depositAddress(ctx context.Context) (string, error) {
	out, err := u.mustDo(ctx, "GET", "/v1/deposit-address", nil, 200, 201)
	if err != nil {
		return "", err
	}
	var r struct{ Address string }
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("decoding %s: %w", out, err)
	}
	u.Deposit = r.Address
	return r.Address, nil
}

type balance struct{ Total, Locked, Available string }

func (u *user) balances(ctx context.Context) (map[string]balance, error) {
	out, err := u.mustDo(ctx, "GET", "/v1/balances", nil, 200)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		AssetID   string `json:"asset_id"`
		Total     string `json:"total_balance"`
		Locked    string `json:"locked_balance"`
		Available string `json:"available_balance"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", out, err)
	}
	m := map[string]balance{}
	for _, r := range rows {
		m[r.AssetID] = balance{r.Total, r.Locked, r.Available}
	}
	return m, nil
}

func (u *user) whitelistStatus(ctx context.Context, addr common.Address) (string, error) {
	out, err := u.mustDo(ctx, "GET", "/v1/whitelist", nil, 200)
	if err != nil {
		return "", err
	}
	var rows []struct{ Address, Status string }
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", fmt.Errorf("decoding %s: %w", out, err)
	}
	for _, r := range rows {
		if strings.EqualFold(r.Address, addr.Hex()) {
			return r.Status, nil
		}
	}
	return "absent", nil
}

func cmdUserCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	n := fs.Int("n", 1, "how many users")
	name, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if name != "" && *n != 1 {
		return errors.New("a name and -n cannot be combined")
	}
	s, err := loadState()
	if err != nil {
		return err
	}
	for i := 0; i < *n; i++ {
		nm := name
		if nm == "" {
			for k := len(s.Users) + 1; ; k++ {
				if nm = fmt.Sprintf("user%d", k); !s.has(nm) {
					break
				}
			}
		}
		if s.has(nm) {
			return fmt.Errorf("user %q already exists", nm)
		}
		resp, err := http.Post(gatewayURL+"/users", "application/json", strings.NewReader("{}"))
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("POST /users: %d %s", resp.StatusCode, body)
		}
		var r struct {
			UserID string `json:"user_id"`
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		u := &user{Name: nm, ID: r.UserID, APIKey: r.APIKey}
		if _, err := u.depositAddress(ctx); err != nil {
			return err
		}
		s.Users = append(s.Users, u)
		if err := s.save(); err != nil { // save per user: the API key is shown once
			return err
		}
		fmt.Printf("created %-10s %s  deposit %s\n", u.Name, u.ID, u.Deposit)
	}
	return nil
}

func (s *state) has(name string) bool { _, err := s.find(name); return err == nil }

func cmdUserList(ctx context.Context) error {
	s, err := loadState()
	if err != nil {
		return err
	}
	if len(s.Users) == 0 {
		fmt.Println("no users yet — `sim user create alice`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tUSER ID\tDEPOSIT ADDRESS\tETH (avail/locked)\tTT (avail/locked)")
	for _, u := range s.Users {
		b, err := u.balances(ctx)
		if err != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t\n", u.Name, u.ID, u.Deposit, err)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Name, u.ID, u.Deposit, pair(b[ethAssetID]), pair(b[tokenAssetID]))
	}
	return tw.Flush()
}

func pair(b balance) string {
	if b.Available == "" && b.Locked == "" {
		return "-"
	}
	return fmt.Sprintf("%s / %s", fmtAmount(b.Available), fmtAmount(b.Locked))
}

// ---- chain ----------------------------------------------------------------------------

type chain struct {
	mu         sync.Mutex // one signer per nonce: concurrent sends from the same Anvil account would collide
	eth        *ethclient.Client
	rpc        *rpc.Client
	key0, key1 *ecdsa.PrivateKey
	token      common.Address
}

func dial(ctx context.Context) (*chain, error) {
	r, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	k0, _ := crypto.HexToECDSA(anvilKey0)
	k1, _ := crypto.HexToECDSA(anvilKey1)
	c := &chain{eth: ethclient.NewClient(r), rpc: r, key0: k0, key1: k1, token: common.HexToAddress(tokenAddress)}
	if _, err := c.eth.ChainID(ctx); err != nil {
		return nil, fmt.Errorf("anvil at %s unreachable (is the stack up? `make up` in e2e/): %w", rpcURL, err)
	}
	return c, nil
}

// ensureReady deploys the test token if the chain has none, and floats the Hot Wallet: it pays
// withdrawals and Gas Top-Ups, so without float nothing a user does can complete.
func (c *chain) ensureReady(ctx context.Context, verbose bool) error {
	say := func(f string, a ...any) {
		if verbose {
			fmt.Printf(f+"\n", a...)
		}
	}
	// Anvil's pre-funded accounts pay every deposit; top them up rather than ever running dry.
	for _, k := range []*ecdsa.PrivateKey{c.key0, c.key1} {
		who := crypto.PubkeyToAddress(k.PublicKey)
		if bal, err := c.eth.BalanceAt(ctx, who, nil); err != nil {
			return err
		} else if bal.Cmp(new(big.Int).Mul(big.NewInt(1000), oneETH)) < 0 {
			full := new(big.Int).Mul(big.NewInt(10_000), oneETH)
			if err := c.rpc.CallContext(ctx, nil, "anvil_setBalance", who, fmt.Sprintf("0x%x", full)); err != nil {
				return fmt.Errorf("anvil_setBalance: %w", err)
			}
			say("refilled Anvil account %s to 10000 ETH", who)
		}
	}
	if code, err := c.eth.CodeAt(ctx, c.token, nil); err != nil {
		return err
	} else if len(code) == 0 {
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(contracts.MockERC20Bin), "0x"))
		if err != nil {
			return err
		}
		u256, _ := abi.NewType("uint256", "", nil)
		arg, _ := abi.Arguments{{Type: u256}}.Pack(new(big.Int).Mul(big.NewInt(1_000_000_000_000), oneETH))
		rc, err := c.send(ctx, c.key0, nil, big.NewInt(0), append(raw, arg...))
		if err != nil {
			return err
		}
		if rc.ContractAddress != c.token {
			return fmt.Errorf("token deployed at %s but e2e/assets.yaml registers %s: account #0 had already sent a tx. Reset the chain: `make down up` in e2e/ and `sim reset`", rc.ContractAddress, c.token)
		}
		say("deployed test token TT at %s (10^12 TT held by Anvil account #0: a long-running simulation must not run out)", c.token)
	}
	hot, err := hotWalletAddress(ctx)
	if err != nil {
		return err
	}
	if bal, err := c.eth.BalanceAt(ctx, hot, nil); err != nil {
		return err
	} else if bal.Cmp(new(big.Int).Mul(big.NewInt(10), oneETH)) < 0 {
		if _, err := c.send(ctx, c.key1, &hot, new(big.Int).Mul(big.NewInt(50), oneETH), nil); err != nil {
			return err
		}
		say("floated Hot Wallet %s with 50 ETH", hot)
	}
	if tt, err := c.tokenBalance(ctx, hot); err != nil {
		return err
	} else if tt.Cmp(new(big.Int).Mul(big.NewInt(1000), oneETH)) < 0 {
		if _, err := c.send(ctx, c.key0, &c.token, big.NewInt(0), transferCalldata(hot, new(big.Int).Mul(big.NewInt(5000), oneETH))); err != nil {
			return err
		}
		say("floated Hot Wallet %s with 5000 TT", hot)
	}
	say("ready: token %s, hot wallet %s", c.token, hot)
	return nil
}

// hotWalletAddress asks wallet-service over mTLS; the endpoint admits only CN=sweeper, so this borrows
// the sweeper's leaf certificate (infra/pki/out/sweeper, from `make certs`).
func hotWalletAddress(ctx context.Context) (common.Address, error) {
	dir := os.Getenv("SIM_PKI_DIR")
	if dir == "" {
		wd, _ := os.Getwd()
		for d := wd; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "infra", "pki", "out", "sweeper")); err == nil {
				dir = filepath.Join(d, "infra", "pki", "out", "sweeper")
				break
			}
			if d == filepath.Dir(d) {
				return common.Address{}, errors.New("infra/pki/out/sweeper not found: run from inside the repo, or set SIM_PKI_DIR (run `make certs` in e2e/ first)")
			}
		}
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return common.Address{}, err
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca-cert.pem"))
	if err != nil {
		return common.Address{}, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "wallet-service", MinVersion: tls.VersionTLS13}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, walletTLS+"/v1/wallet/hot-address", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return common.Address{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct{ Address string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &r) != nil {
		return common.Address{}, fmt.Errorf("hot-address: %d %s (was the wallet seeded? `make up` does it)", resp.StatusCode, body)
	}
	return common.HexToAddress(r.Address), nil
}

func (c *chain) send(ctx context.Context, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) (*types.Receipt, error) {
	tx, err := c.submit(ctx, key, to, value, data)
	if err != nil {
		return nil, err
	}
	for { // Anvil mines on every tx (automine) or on its block time
		rc, err := c.eth.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			if rc.Status != types.ReceiptStatusSuccessful {
				return nil, fmt.Errorf("tx %s reverted", tx.Hash())
			}
			return rc, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// submit signs and broadcasts one tx. Serialised, so the pending nonce it reads is still free when it sends.
func (c *chain) submit(ctx context.Context, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) (*types.Transaction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := c.eth.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tip := big.NewInt(1e9)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	gas := uint64(3_000_000)
	if to != nil {
		if gas, err = c.eth.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data}); err != nil {
			return nil, fmt.Errorf("estimating gas: %w", err)
		}
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap, Gas: gas, To: to, Value: value, Data: data})
	if err != nil {
		return nil, err
	}
	if err := c.eth.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("sending tx: %w", err)
	}
	return tx, nil
}

func (c *chain) mine(ctx context.Context, n int) error {
	if err := c.rpc.CallContext(ctx, nil, "anvil_mine", fmt.Sprintf("0x%x", n)); err != nil {
		return fmt.Errorf("anvil_mine: %w", err)
	}
	return nil
}

func (c *chain) tokenBalance(ctx context.Context, who common.Address) (*big.Int, error) {
	data := append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(who.Bytes(), 32)...)
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

func transferCalldata(to common.Address, amount *big.Int) []byte {
	addr, _ := abi.NewType("address", "", nil)
	u256, _ := abi.NewType("uint256", "", nil)
	args, _ := abi.Arguments{{Type: addr}, {Type: u256}}.Pack(to, amount)
	return append(crypto.Keccak256([]byte("transfer(address,uint256)"))[:4], args...)
}

// ---- commands -------------------------------------------------------------------------

// parseArgs splits positionals from flags in any order (the std flag package stops at the first positional).
func parseArgs(fs *flag.FlagSet, args []string, wantPos int) ([]string, error) {
	var pos, flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "--no-wait" && a != "-no-wait" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	if len(pos) != wantPos {
		return nil, fmt.Errorf("expected %d arguments, got %d", wantPos, len(pos))
	}
	return pos, nil
}

func cmdDeposit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deposit", flag.ContinueOnError)
	noWait := fs.Bool("no-wait", false, "do not wait for the ledger credit")
	pos, err := parseArgs(fs, args, 3)
	if err != nil {
		return fmt.Errorf("%w\nusage: sim deposit <user> <eth|tt> <amount> [--no-wait]", err)
	}
	s, err := loadState()
	if err != nil {
		return err
	}
	u, err := s.find(pos[0])
	if err != nil {
		return err
	}
	a, err := lookupAsset(pos[1])
	if err != nil {
		return err
	}
	amt, err := parseAmount(pos[2])
	if err != nil {
		return err
	}
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	if err := c.ensureReady(ctx, false); err != nil {
		return err
	}
	dep, err := u.depositAddress(ctx)
	if err != nil {
		return err
	}
	before, err := u.balances(ctx)
	if err != nil {
		return err
	}
	to := common.HexToAddress(dep)
	var rc *types.Receipt
	if a.id == ethAssetID {
		rc, err = c.send(ctx, c.key1, &to, amt, nil)
	} else {
		rc, err = c.send(ctx, c.key0, &c.token, big.NewInt(0), transferCalldata(to, amt))
	}
	if err != nil {
		return err
	}
	fmt.Printf("sent %s %s to %s's deposit address %s (tx %s, block %d)\n", fmtAmount(amt.String()), a.symbol, u.Name, dep, rc.TxHash, rc.BlockNumber)
	if err := c.mine(ctx, confirmationDepth+3); err != nil {
		return err
	}
	if *noWait {
		return nil
	}
	want := new(big.Int).Add(bigOf(before[a.id].Total), amt)
	return waitFor(ctx, 90*time.Second, "the ledger credit", func() (bool, string, error) {
		b, err := u.balances(ctx)
		if err != nil {
			return false, "", err
		}
		if err := c.mine(ctx, 1); err != nil { // keep the chain moving for chain-watcher's depth
			return false, "", err
		}
		return bigOf(b[a.id].Total).Cmp(want) >= 0, "total " + fmtAmount(b[a.id].Total), nil
	}, func() { fmt.Printf("credited: %s now has %s %s\n", u.Name, fmtAmount(want.String()), a.symbol) })
}

func cmdWithdraw(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("withdraw", flag.ContinueOnError)
	toFlag := fs.String("to", "", "destination address (default: a fresh random one)")
	noWait := fs.Bool("no-wait", false, "return once the request is accepted")
	pos, err := parseArgs(fs, args, 3)
	if err != nil {
		return fmt.Errorf("%w\nusage: sim withdraw <user> <eth|tt> <amount> [--to 0x..] [--no-wait]", err)
	}
	s, err := loadState()
	if err != nil {
		return err
	}
	u, err := s.find(pos[0])
	if err != nil {
		return err
	}
	a, err := lookupAsset(pos[1])
	if err != nil {
		return err
	}
	amt, err := parseAmount(pos[2])
	if err != nil {
		return err
	}
	var dest common.Address
	if *toFlag != "" {
		if !common.IsHexAddress(*toFlag) {
			return fmt.Errorf("bad --to address %q", *toFlag)
		}
		dest = common.HexToAddress(*toFlag)
	} else {
		k, _ := crypto.GenerateKey()
		dest = crypto.PubkeyToAddress(k.PublicKey)
	}
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	if err := c.ensureReady(ctx, false); err != nil {
		return err
	}

	// Whitelist (a new entry is `pending` for the cooldown, 2s in this stack).
	st, err := u.whitelistStatus(ctx, dest)
	if err != nil {
		return err
	}
	if st == "absent" {
		if _, err := u.mustDo(ctx, "POST", "/v1/whitelist", map[string]string{"address": dest.Hex()}, 201); err != nil {
			return err
		}
		fmt.Printf("whitelisted %s\n", dest)
		st = "pending"
	}
	if st != "active" {
		err := waitFor(ctx, 60*time.Second, "the whitelist cooldown", func() (bool, string, error) {
			st, err := u.whitelistStatus(ctx, dest)
			return st == "active", st, err
		}, func() {})
		if err != nil {
			return err
		}
	}

	key := fmt.Sprintf("sim-%d", time.Now().UnixNano())
	out, err := u.mustDo(ctx, "POST", "/v1/withdrawals", map[string]string{
		"idempotency_key": key, "asset_id": a.id, "amount": amt.String(), "to_address": dest.Hex()}, 200, 202)
	if err != nil {
		return err
	}
	fmt.Printf("withdrawal accepted: %s %s %s -> %s (key %s) %s\n", u.Name, fmtAmount(amt.String()), a.symbol, dest, key, strings.TrimSpace(string(out)))
	if *noWait {
		return nil
	}
	var hash string
	err = waitFor(ctx, 120*time.Second, "the withdrawal to confirm", func() (bool, string, error) {
		if err := c.mine(ctx, 1); err != nil { // Withdrawal Confirmation Depth is 2: each poll adds a block
			return false, "", err
		}
		body, err := u.mustDo(ctx, "GET", "/v1/withdrawals/"+key, nil, 200)
		if err != nil {
			return false, "", err
		}
		var r struct {
			Status       string  `json:"status"`
			TxHash       *string `json:"tx_hash"`
			RejectReason *string `json:"reject_reason"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return false, "", err
		}
		if r.TxHash != nil {
			hash = *r.TxHash
		}
		switch r.Status {
		case "failed", "rejected", "disputed":
			return false, "", fmt.Errorf("withdrawal ended %s: %s", r.Status, body)
		}
		return r.Status == "confirmed" && hash != "", r.Status + " " + hash, nil
	}, func() {})
	if err != nil {
		return err
	}
	var onchain *big.Int
	if a.id == ethAssetID {
		onchain, err = c.eth.BalanceAt(ctx, dest, nil)
	} else {
		onchain, err = c.tokenBalance(ctx, dest)
	}
	if err != nil {
		return err
	}
	fmt.Printf("confirmed: tx %s — %s now holds %s %s on chain\n", hash, dest, fmtAmount(onchain.String()), a.symbol)
	return nil
}

// ---- small helpers --------------------------------------------------------------------

func waitFor(ctx context.Context, d time.Duration, what string, ok func() (bool, string, error), done func()) error {
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		fin, l, err := ok()
		if err != nil {
			return err
		}
		if fin {
			done()
			return nil
		}
		last = l
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out after %s waiting for %s; last seen: %s", d, what, last)
}

func bigOf(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}

// parseAmount reads a decimal such as "0.5" as an 18-decimals integer amount.
func parseAmount(s string) (*big.Int, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 18 {
		return nil, fmt.Errorf("amount %q has more than 18 decimals", s)
	}
	n, ok := new(big.Int).SetString(whole+frac+strings.Repeat("0", 18-len(frac)), 10)
	if !ok || n.Sign() <= 0 {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	return n, nil
}

// fmtAmount renders an 18-decimals integer string as a decimal with trailing zeros trimmed.
func fmtAmount(s string) string {
	if s == "" {
		return "0"
	}
	n := bigOf(s)
	whole, rem := new(big.Int).QuoRem(n, oneETH, new(big.Int))
	if rem.Sign() == 0 {
		return whole.String()
	}
	return whole.String() + "." + strings.TrimRight(fmt.Sprintf("%018s", rem.String()), "0")
}

func postJSON(ctx context.Context, url, body string, want int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return nil, fmt.Errorf("POST %s: %d %s", url, resp.StatusCode, out)
	}
	return out, nil
}
