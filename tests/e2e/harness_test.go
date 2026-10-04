// Package e2e is the QA black-box suite: it drives the real wallet-service and ledger-service
// (docker compose, tests/e2e/stack.sh) over their real mTLS APIs, puts real transactions on anvil,
// and then checks the Postgres rows. Run: E2E=1 go test ./... (see README in docs/qa).
//
// Three things in here are harness STAND-INS for services that don't exist yet (M2/M3):
//   - chain-watcher  -> watcher.go style helpers (scanBlock / publishDeposit)
//   - sweeper        -> sweepNative / gasTopUp
//   - withdrawal-svc -> withdrawal steps (lock, sign, broadcast, settle)
// They only speak the documented contracts, so the ledger/wallet side is tested for real.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"
)

const (
	ethAsset = "11111111-1111-1111-1111-111111111111"
	ttAsset  = "22222222-2222-2222-2222-222222222222"
	ttAddr   = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	chainID  = 31337

	rpcURL    = "http://localhost:18545"
	walletURL = "http://localhost:18080"
	walletTLS = "https://localhost:18443"
	ledgerTLS = "https://localhost:18444"
	ledgerHTP = "http://localhost:18081"
	ledgerDSN = "postgres://custody:custody@localhost:15432/ledger?sslmode=disable"
	walletDSN = "postgres://custody:custody@localhost:15432/custody?sslmode=disable"

	// anvil's well-known account #0 — the faucet for everything in this suite.
	faucetKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
)

var (
	eth      = new(big.Int).SetUint64(1e18)
	faucet   *ecdsa.PrivateKey
	cl       *ethclient.Client
	rawRPC   *rpc.Client
	ldb, wdb *pgxpool.Pool
	hotAddr  common.Address
	hotKey   *ecdsa.PrivateKey
	userSeed *hdw.Wallet
	repoRoot string
	faucetMu chan struct{} // serialises faucet nonce use
)

func TestMain(m *testing.M) {
	if os.Getenv("E2E") == "" {
		fmt.Println("E2E not set — skipping e2e suite (needs tests/e2e/stack.sh up)")
		os.Exit(0)
	}
	ctx := context.Background()
	var err error
	must := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, "harness setup:", e)
			os.Exit(2)
		}
	}
	repoRoot, _ = os.Getwd()
	repoRoot = strings.TrimSuffix(repoRoot, "/tests/e2e")
	faucet, err = crypto.HexToECDSA(faucetKeyHex)
	must(err)
	faucetMu = make(chan struct{}, 1)
	rawRPC, err = rpc.DialContext(ctx, rpcURL)
	must(err)
	cl = ethclient.NewClient(rawRPC)
	ldb, err = pgxpool.New(ctx, ledgerDSN)
	must(err)
	wdb, err = pgxpool.New(ctx, walletDSN)
	must(err)

	mn := loadMnemonics()
	hot, err := hdw.NewFromMnemonic(mn["hot_wallet"])
	must(err)
	acc, err := hot.Derive(hdw.MustParseDerivationPath("m/44'/60'/0'/0/0"), false)
	must(err)
	hotAddr = acc.Address
	hotKey, err = hot.PrivateKey(acc)
	must(err)
	userSeed, err = hdw.NewFromMnemonic(mn["user_derivation"])
	must(err)

	must(ensureToken(ctx))
	// The hot wallet needs working capital. Idempotent: only tops up if low.
	if b, _ := cl.BalanceAt(ctx, hotAddr, nil); b.Cmp(new(big.Int).Mul(big.NewInt(500), eth)) < 0 {
		must(rawRPC.CallContext(ctx, nil, "anvil_setBalance", hotAddr.Hex(), hexBig(new(big.Int).Mul(big.NewInt(1000), eth))))
	}
	if bal := tokenBalance(common.HexToAddress(ttAddr), hotAddr); bal.Sign() == 0 {
		mintTT(hotAddr, new(big.Int).Mul(big.NewInt(1_000_000), eth))
	}
	os.Exit(m.Run())
}

func loadMnemonics() map[string]string {
	raw, err := os.ReadFile("tests/e2e/.state/mnemonics.env")
	if err != nil {
		raw, err = os.ReadFile(".state/mnemonics.env")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "no mnemonics (run stack.sh up):", err)
		os.Exit(2)
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[strings.TrimPrefix(k, "MNEMONIC_")] = strings.Trim(v, `"`)
		}
	}
	return out
}

func hexBig(v *big.Int) string { return "0x" + v.Text(16) }

// ---------------------------------------------------------------- anvil helpers

func anvil(t testing.TB, method string, params ...any) {
	t.Helper()
	if err := rawRPC.CallContext(context.Background(), nil, method, params...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func mine(t testing.TB, n int) { t.Helper(); anvil(t, "anvil_mine", hexBig(big.NewInt(int64(n)))) }

func head(t testing.TB) uint64 {
	t.Helper()
	n, err := cl.BlockNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// sendFrom signs and broadcasts with an arbitrary key using the live pending nonce. Returns the tx.
func sendFrom(t testing.TB, key *ecdsa.PrivateKey, to common.Address, value *big.Int, data []byte) *types.Transaction {
	t.Helper()
	ctx := context.Background()
	from := crypto.PubkeyToAddress(key.PublicKey)
	faucetMu <- struct{}{}
	defer func() { <-faucetMu }()
	nonce, err := cl.PendingNonceAt(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	gas := uint64(100000)
	if len(data) == 0 {
		gas = 21000
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(chainID)), &types.DynamicFeeTx{
		ChainID: big.NewInt(chainID), Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9),
		Gas: gas, To: &to, Value: value, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.SendTransaction(ctx, tx); err != nil {
		t.Fatalf("send: %v", err)
	}
	return tx
}

func receipt(t testing.TB, h common.Hash) *types.Receipt {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if r, err := cl.TransactionReceipt(ctx, h); err == nil {
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no receipt for %s", h)
	return nil
}

func fundETH(t testing.TB, to common.Address, v *big.Int) *types.Receipt {
	t.Helper()
	return receipt(t, sendFrom(t, faucet, to, v, nil).Hash())
}

func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

func word(v *big.Int) []byte { return common.LeftPadBytes(v.Bytes(), 32) }

func mintTT(to common.Address, v *big.Int) {
	data := append(selector("mint(address,uint256)"), append(common.LeftPadBytes(to.Bytes(), 32), word(v)...)...)
	t := &testing.T{}
	_ = t
	ctx := context.Background()
	nonce, _ := cl.PendingNonceAt(ctx, crypto.PubkeyToAddress(faucet.PublicKey))
	tc := common.HexToAddress(ttAddr)
	tx, _ := types.SignNewTx(faucet, types.LatestSignerForChainID(big.NewInt(chainID)), &types.DynamicFeeTx{
		ChainID: big.NewInt(chainID), Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 120000, To: &tc, Data: data})
	if err := cl.SendTransaction(ctx, tx); err != nil {
		fmt.Fprintln(os.Stderr, "mint:", err)
		os.Exit(2)
	}
	for i := 0; i < 50; i++ {
		if _, err := cl.TransactionReceipt(ctx, tx.Hash()); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func tokenBalance(token, who common.Address) *big.Int {
	out, err := cl.CallContract(context.Background(), ethCallMsg(token, append(selector("balanceOf(address)"), common.LeftPadBytes(who.Bytes(), 32)...)), nil)
	if err != nil || len(out) == 0 {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(out)
}

func ensureToken(ctx context.Context) error {
	if code, _ := cl.CodeAt(ctx, common.HexToAddress(ttAddr), nil); len(code) > 0 {
		return nil
	}
	n, _ := cl.NonceAt(ctx, crypto.PubkeyToAddress(faucet.PublicKey), nil)
	if n != 0 {
		return fmt.Errorf("QA token not at %s and faucet nonce is %d: anvil must be fresh (stack.sh reset)", ttAddr, n)
	}
	_, err := deploy(ctx, "TT")
	return err
}

func deploy(ctx context.Context, name string) (common.Address, error) {
	raw, err := os.ReadFile(fmt.Sprintf("%s/tests/e2e/contracts/%s.bin", repoRoot, name))
	if err != nil {
		return common.Address{}, err
	}
	code := common.FromHex(strings.TrimSpace(string(raw)))
	nonce, _ := cl.PendingNonceAt(ctx, crypto.PubkeyToAddress(faucet.PublicKey))
	tx, err := types.SignNewTx(faucet, types.LatestSignerForChainID(big.NewInt(chainID)), &types.DynamicFeeTx{
		ChainID: big.NewInt(chainID), Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 3_000_000, Data: code})
	if err != nil {
		return common.Address{}, err
	}
	if err := cl.SendTransaction(ctx, tx); err != nil {
		return common.Address{}, err
	}
	for i := 0; i < 100; i++ {
		if r, err := cl.TransactionReceipt(ctx, tx.Hash()); err == nil {
			return r.ContractAddress, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return common.Address{}, fmt.Errorf("deploy %s: no receipt", name)
}

// ---------------------------------------------------------------- HTTP clients

func mtls(cn string) *http.Client {
	cert, err := tls.LoadX509KeyPair(fmt.Sprintf("%s/infra/pki/out/%s/cert.pem", repoRoot, cn), fmt.Sprintf("%s/infra/pki/out/%s/key.pem", repoRoot, cn))
	if err != nil {
		panic(err)
	}
	ca, _ := os.ReadFile(repoRoot + "/tests/e2e/.state/ca/ca-cert.pem")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13},
		MaxIdleConnsPerHost: 64,
	}}
}

var clients = map[string]*http.Client{}

func client(cn string) *http.Client {
	if c, ok := clients[cn]; ok {
		return c
	}
	c := mtls(cn)
	clients[cn] = c
	return c
}

type resp struct {
	Code int
	Body []byte
	M    map[string]any
}

func (r resp) s(k string) string { v, _ := r.M[k].(string); return v }

func do(c *http.Client, method, url string, body any) resp {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return resp{Code: -1, Body: []byte(err.Error())}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{Code: res.StatusCode, Body: b}
	_ = json.Unmarshal(b, &r.M)
	return r
}

var uniq atomic.Int64

func uid(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, uniq.Add(1))
}

// ---------------------------------------------------------------- wallet-service

func depositAddress(t testing.TB, user string) common.Address {
	t.Helper()
	r := do(http.DefaultClient, "POST", walletURL+"/v1/users/"+user+"/deposit-address", nil)
	if r.Code != 200 && r.Code != 201 {
		t.Fatalf("deposit-address(%s): %d %s", user, r.Code, r.Body)
	}
	return common.HexToAddress(r.s("address"))
}

func ownerOf(addr common.Address) resp {
	return do(client("chain-watcher"), "GET", walletTLS+"/v1/addresses/"+addr.Hex(), nil)
}

// userKey recovers the private key of a user's deposit address from the user_derivation mnemonic
// (QA holds it; only possible because this is a test seed).
func userKey(t testing.TB, addr common.Address) *ecdsa.PrivateKey {
	t.Helper()
	for i := uint32(0); i < 5000; i++ {
		a, err := userSeed.Derive(hdw.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/0'/0/%d", i)), false)
		if err != nil {
			t.Fatal(err)
		}
		if a.Address == addr {
			k, _ := userSeed.PrivateKey(a)
			return k
		}
	}
	t.Fatalf("address %s not derivable from user seed in first 5000 indices", addr)
	return nil
}

func signHot(key, to, asset, amount string) resp {
	return do(client("withdrawal-service"), "POST", walletTLS+"/v1/sign/hot-wallet", map[string]any{
		"to_address": to, "asset_id": asset, "amount": amount, "chain_id": chainID, "idempotency_key": key})
}

func broadcast(t testing.TB, signedHex string) common.Hash {
	t.Helper()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(common.FromHex(signedHex)); err != nil {
		t.Fatalf("decode signed tx: %v", err)
	}
	if err := cl.SendTransaction(context.Background(), &tx); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	return tx.Hash()
}

// ---------------------------------------------------------------- ledger-service

func ledgerLock(key, user, asset, amount string) resp {
	return do(client("withdrawal-service"), "POST", ledgerTLS+"/v1/ledger/locks", map[string]any{
		"idempotency_key": key, "user_id": user, "asset_id": asset, "amount": amount})
}
func ledgerSettle(key string) resp {
	return do(client("withdrawal-service"), "POST", ledgerTLS+"/v1/ledger/locks/"+key+"/settle", nil)
}
func ledgerRelease(key string) resp {
	return do(client("withdrawal-service"), "POST", ledgerTLS+"/v1/ledger/locks/"+key+"/release", nil)
}

type bal struct{ Total, Locked, Avail *big.Int }

func balance(t testing.TB, user, asset string) bal {
	t.Helper()
	var tot, lk string
	err := ldb.QueryRow(context.Background(),
		`SELECT total_balance::text, locked_balance::text FROM accounts WHERE kind='user' AND user_id=$1 AND asset_id=$2`, user, asset).Scan(&tot, &lk)
	if err != nil {
		return bal{new(big.Int), new(big.Int), new(big.Int)}
	}
	a, _ := new(big.Int).SetString(tot, 10)
	b, _ := new(big.Int).SetString(lk, 10)
	return bal{a, b, new(big.Int).Sub(a, b)}
}

func wantBal(t testing.TB, user, asset string, total, locked int64) {
	t.Helper()
	b := balance(t, user, asset)
	if b.Total.Int64() != total || b.Locked.Int64() != locked {
		t.Fatalf("%s balance total/locked = %s/%s, want %d/%d", user, b.Total, b.Locked, total, locked)
	}
}

func q1[T any](t testing.TB, db *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

// ---------------------------------------------------------------- stand-in chain-watcher / sweeper

type depositEvent struct {
	Version     int    `json:"version"`
	ChainID     int64  `json:"chain_id"`
	TxHash      string `json:"tx_hash"`
	LogIndex    int64  `json:"log_index"`
	AssetID     string `json:"asset_id"`
	FromAddress string `json:"from_address"`
	ToAddress   string `json:"to_address"`
	UserID      string `json:"user_id"`
	Amount      string `json:"amount"`
	BlockNumber int64  `json:"block_number"`
}

func (e depositEvent) key() string { return fmt.Sprintf("%d:%s:%d", e.ChainID, strings.ToLower(e.TxHash), e.LogIndex) }

// publish sends events to deposits.confirmed through the real Kafka broker.
func publish(t testing.TB, evs ...depositEvent) {
	t.Helper()
	var sb strings.Builder
	for _, e := range evs {
		j, _ := json.Marshal(e)
		fmt.Fprintf(&sb, "%s|%s\n", e.key(), j)
	}
	cmd := exec.Command("docker", "exec", "-i", "digiwal-qa-kafka-1", "/opt/kafka/bin/kafka-console-producer.sh",
		"--bootstrap-server", "localhost:9092", "--topic", "deposits.confirmed",
		"--property", "parse.key=true", "--property", "key.separator=|")
	cmd.Stdin = strings.NewReader(sb.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kafka publish: %v\n%s", err, out)
	}
}

// scanBlock is the stand-in chain-watcher: every native transfer / registered-token Transfer log in
// block n whose recipient is one of OUR deposit addresses becomes a DepositConfirmed event. This
// follows deposit-flow.md literally (it defines no sender exclusion — see DEP-24).
// It enforces Confirmation Depth by refusing blocks not yet 32 deep.
func scanBlock(t testing.TB, n uint64) []depositEvent {
	t.Helper()
	ctx := context.Background()
	if h := head(t); h < n+32 {
		t.Fatalf("stand-in watcher: block %d only has %d confirmations", n, h-n)
	}
	blk, err := cl.BlockByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		t.Fatal(err)
	}
	var out []depositEvent
	transfer := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	for _, tx := range blk.Transactions() {
		rc, err := cl.TransactionReceipt(ctx, tx.Hash())
		if err != nil || rc.Status != 1 {
			continue
		}
		from, _ := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), tx)
		if tx.To() != nil && tx.Value().Sign() > 0 {
			if r := ownerOf(*tx.To()); r.Code == 200 {
				out = append(out, depositEvent{1, chainID, tx.Hash().Hex(), -1, ethAsset, from.Hex(), tx.To().Hex(), r.s("user_id"), tx.Value().String(), int64(n)})
			}
		}
		for _, lg := range rc.Logs {
			if lg.Address != common.HexToAddress(ttAddr) || len(lg.Topics) != 3 || lg.Topics[0] != transfer {
				continue
			}
			to := common.BytesToAddress(lg.Topics[2].Bytes())
			if r := ownerOf(to); r.Code == 200 {
				out = append(out, depositEvent{1, chainID, tx.Hash().Hex(), int64(lg.Index), ttAsset,
					common.BytesToAddress(lg.Topics[1].Bytes()).Hex(), to.Hex(), r.s("user_id"), new(big.Int).SetBytes(lg.Data).String(), int64(n)})
			}
		}
	}
	return out
}

// depositNative: fund a user's deposit address from the faucet and publish the confirmed event.
func depositNative(t testing.TB, user string, amount *big.Int) (depositEvent, common.Address) {
	t.Helper()
	addr := depositAddress(t, user)
	rc := fundETH(t, addr, amount)
	mine(t, 32)
	evs := scanBlock(t, rc.BlockNumber.Uint64())
	for _, e := range evs {
		if strings.EqualFold(e.ToAddress, addr.Hex()) && e.TxHash == rc.TxHash.Hex() {
			publish(t, e)
			return e, addr
		}
	}
	t.Fatalf("stand-in watcher saw no deposit for %s in block %d", addr, rc.BlockNumber)
	return depositEvent{}, addr
}

func waitFor(t testing.TB, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func waitCredited(t testing.TB, user, asset string, total int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s total=%d", user, total), 20*time.Second, func() bool {
		return balance(t, user, asset).Total.Int64() == total
	})
}

func waitDeposit(t testing.TB, e depositEvent) {
	t.Helper()
	waitFor(t, "deposit "+e.key(), 20*time.Second, func() bool {
		var n int
		ldb.QueryRow(context.Background(), `SELECT count(*) FROM ledger_transactions WHERE idempotency_key=$1`, e.key()).Scan(&n)
		if n > 0 {
			return true
		}
		var f int
		ldb.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM flagged_deposits WHERE tx_hash=$1)+(SELECT count(*) FROM failed_events WHERE message_key=$2)`, strings.ToLower(e.TxHash), e.key()).Scan(&f)
		return f > 0
	})
}

func hexTx(tx *types.Transaction) string { b, _ := tx.MarshalBinary(); return "0x" + hex.EncodeToString(b) }
func ethCallMsg(to common.Address, data []byte) (m ethCall) { m.To, m.Data = &to, data; return }

type ethCall = ethereumCallMsg
