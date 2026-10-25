//go:build gatewayit

package signing

// Integration test: wallet-service's signer against the REAL hsm-gateway (a Docker container with a real
// SoftHSMv2 token, real mTLS with a pinned client leaf, a real Signing Policy and Audit Log), with the
// signatures mined by go-ethereum's simulated EVM. Needs Docker and the repo root as the build context:
//
//	cd services/wallet-service && go test -tags gatewayit -count=1 -v -run GatewayIT ./internal/signing
//
// HSM_GATEWAY_IMAGE=name uses an already built image instead of building one.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"

	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

func docker(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out.String(), errb.String())
	}
	return strings.TrimSpace(out.String())
}

func TestGatewayIT_RealGatewayRealSoftHSM(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	image := os.Getenv("HSM_GATEWAY_IMAGE")
	if image == "" {
		image = "hsm-gateway:wallet-it"
		root, _ := filepath.Abs("../../../..")
		docker(t, "", "build", "-q", "-f", filepath.Join(root, "services/hsm-gateway/Dockerfile"), "-t", image, root)
	}

	// --- PKI: a CA, the gateway's leaf, wallet-service's leaf (the only pinned caller)
	dir := t.TempDir()
	ca, caCert, caKey := newTestCA(t)
	wr := func(rel string, b []byte, mode os.FileMode) string {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o777)
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gwCrt, gwKey := ca.issue(t, caCert, caKey, "hsm-gateway")
	wsCrt, wsKey := ca.issue(t, caCert, caKey, "wallet-service")
	wr("certs/cert.pem", gwCrt, 0o644)
	wr("certs/key.pem", gwKey, 0o644)
	wr("certs/ca-cert.pem", ca.certPEM, 0o644)
	cf, kf, caf := wr("client/cert.pem", wsCrt, 0o644), wr("client/key.pem", wsKey, 0o600), wr("client/ca.pem", ca.certPEM, 0o644)
	der := mustDER(t, wsCrt)
	fp := sha256.Sum256(der)
	wr("config/callers.yaml", []byte(fmt.Sprintf("callers:\n  - cn: wallet-service\n    sha256: %q\n", hex.EncodeToString(fp[:]))), 0o644)
	wr("secrets/pin", []byte("it-user-pin-4711\n"), 0o644)
	// The writable state lives in named volumes: on a Docker Desktop host bind mount SoftHSM (uid 10001) cannot
	// create its token files (CKR_GENERAL_ERROR), and a fresh named volume is root-owned, so chown it once.
	vol := fmt.Sprintf("hsmgw-it-%d", time.Now().UnixNano())
	for _, d := range []string{"tokens", "audit", "state"} {
		docker(t, "", "volume", "create", vol+"-"+d)
		docker(t, "", "run", "--rm", "--user", "0:0", "--entrypoint", "chown", "-v", vol+"-"+d+":/d", image, "-R", "10001:10001", "/d")
		d := d
		t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol+"-"+d).Run() })
	}

	mounts := []string{
		"-v", dir + "/certs:/certs:ro", "-v", dir + "/config:/config:ro", "-v", dir + "/secrets/pin:/run/secrets/hsm-user-pin:ro",
		"-v", vol + "-tokens:/var/lib/softhsm/tokens", "-v", vol + "-audit:/var/lib/hsm-gateway/audit", "-v", vol + "-state:/var/lib/hsm-gateway/state",
	}
	env := []string{"-e", "TLS_CERT_FILE=/certs/cert.pem", "-e", "TLS_KEY_FILE=/certs/key.pem", "-e", "TLS_CA_FILE=/certs/ca-cert.pem",
		"-e", "CALLERS_FILE=/config/callers.yaml", "-e", "POLICY_FILE=/config/policy.yaml", "-e", "POLICY_REQUIRE_ROOT_OWNER=false"}

	// --- ceremony: init the token and generate the Hot Key inside it
	out := docker(t, "so-pin-it-0815\n", append(append([]string{"run", "--rm", "-i", "--user", "10001:10001"}, mounts...), append(env, image, "init-token")...)...)
	m := regexp.MustCompile(`hot_address=(0x[0-9a-fA-F]{40})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no hot_address in init-token output:\n%s", out)
	}
	hotAddr := common.HexToAddress(m[1])

	// --- the simulated chain, with the HSM's address funded (chain id 1337)
	backend := simulated.NewBackend(types.GenesisAlloc{hotAddr: {Balance: new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	client := backend.Client()
	ctx := context.Background()
	id, _ := client.ChainID(ctx)

	policy := fmt.Sprintf(`enabled: true
chain_id: %d
hot_address: %q
warm_address: "0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"
denied_recipients: []
native:
  max_per_tx: "5000000000000000000"
  treasury_max_per_tx: "120000000000000000000"
  window_1h: "9000000000000000000"
  window_24h: "120000000000000000000"
tokens: []
`, id.Int64(), hotAddr.Hex())
	wr("config/policy.yaml", []byte(policy), 0o644)

	name := fmt.Sprintf("hsmgw-it-%d", time.Now().UnixNano())
	// a fixed host port: a published ephemeral port changes when the container is restarted (step 4)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	docker(t, "", append(append([]string{"run", "-d", "--name", name, "--user", "10001:10001", "-p", "127.0.0.1:" + hostPort + ":8443"}, mounts...), append(env, image)...)...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Logf("%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	tlsCfg, err := GatewayTLSConfig(cf, kf, caf)
	if err != nil {
		t.Fatal(err)
	}
	var g *GatewayHot
	for i := 0; i < 40; i++ {
		g, err = NewGatewayHot(ctx, GatewayConfig{BaseURL: "https://127.0.0.1:" + hostPort, TLS: tlsCfg, CallerCN: "wallet-service"})
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("gateway never came up: %v", err)
	}
	if a, _ := g.HotAddress(ctx); a != hotAddr || g.PinnedChainID() != id.Uint64() {
		t.Fatalf("gateway key %s chain %d, ceremony said %s chain %d", a, g.PinnedChainID(), hotAddr, id)
	}

	store, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	s, err := NewWithHot(ctx, store, g, simChain{client}, Assets{nativeID: {Type: AssetNative}})
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress(dest)

	// 1. payouts: sent to the EVM, mined
	for i, key := range []string{"withdrawal:a", "withdrawal:b"} {
		resp, err := s.Sign(ctx, wdCN, Request{ToAddress: dest, AssetID: nativeID, Amount: "1000000000000000", ChainID: id.Int64(), IdempotencyKey: key})
		if err != nil {
			t.Fatalf("payout %d: %v", i, err)
		}
		if resp.Nonce != uint64(i) {
			t.Fatalf("nonce %d, want %d", resp.Nonce, i)
		}
		if err := client.SendTransaction(ctx, decode(t, resp)); err != nil {
			t.Fatalf("EVM rejected HSM-signed payout %d: %v", i, err)
		}
	}
	// 2. a Fee Bump of the second one (same nonce, higher fees) and a Nonce Filler for it: both accepted into the pool
	bump, err := s.Replace(ctx, wdCN, "withdrawal:b", ReplaceRequest{ReplacementSeq: 1})
	if err != nil {
		t.Fatalf("fee bump: %v", err)
	}
	if err := client.SendTransaction(ctx, txOf(t, bump.SignedRawTx)); err != nil {
		t.Fatalf("EVM rejected the HSM-signed Fee Bump: %v", err)
	}
	filler, err := s.Cancel(ctx, "withdrawal:b")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := client.SendTransaction(ctx, txOf(t, hexTx(filler.SignedRawTx))); err != nil {
		t.Fatalf("EVM rejected the HSM-signed Nonce Filler: %v", err)
	}
	backend.Commit()
	if n, _ := client.NonceAt(ctx, hotAddr, nil); n != 2 {
		t.Fatalf("on-chain nonce %d, want 2", n)
	}
	if bal, _ := client.BalanceAt(ctx, to, nil); bal.Cmp(big.NewInt(1e15)) != 0 {
		t.Fatalf("recipient has %s, want exactly the first payout (the second was cancelled by its filler)", bal)
	}
	if r, _ := client.TransactionReceipt(ctx, common.HexToHash(filler.TxHash)); r == nil {
		t.Fatal("the Nonce Filler was not the tx mined at nonce 1")
	}

	// 3. policy refusal: 6 ETH > max_per_tx 5 ETH. Typed error, no nonce consumed, the breaker stays closed.
	_, err = s.Sign(ctx, wdCN, Request{ToAddress: dest, AssetID: nativeID, Amount: "6000000000000000000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:big"})
	var pr *PolicyRefusedError
	if !errors.As(err, &pr) || pr.Rule != "max_per_tx" {
		t.Fatalf("got %v, want policy refusal max_per_tx", err)
	}
	if err := g.Available(); err != nil {
		t.Fatalf("breaker opened on a refusal: %v", err)
	}
	// ... and a recipient the policy never allows (the Hot address itself)
	_, err = s.Sign(ctx, wdCN, Request{ToAddress: hotAddr.Hex(), AssetID: nativeID, Amount: "1000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:self"})
	if !errors.As(err, &pr) || pr.Rule != "recipient_hot" {
		t.Fatalf("got %v, want policy refusal recipient_hot", err)
	}

	// 4. the gateway dies: 503-class error, nonce not consumed, and after it is back the same key signs the same nonce
	docker(t, "", "kill", name)
	_, err = s.Sign(ctx, wdCN, Request{ToAddress: dest, AssetID: nativeID, Amount: "1000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:c"})
	if !errors.Is(err, ErrSignerUnavailable) {
		t.Fatalf("with the gateway killed: %v, want ErrSignerUnavailable", err)
	}
	if _, err := store.LookupHotSignature(ctx, wdCN, "withdrawal:c", requestHash(Request{ToAddress: dest, AssetID: nativeID, Amount: "1000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:c"})); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a signature was stored while the gateway was down: %v", err)
	}
	st, _ := store.NonceStates(ctx, s.HotKey())
	if len(st) != 1 || st[0].NextNonce != 2 {
		t.Fatalf("nonce state after a failed sign: %+v, want next_nonce 2", st)
	}
	docker(t, "", "start", name)
	time.Sleep(breakerOpenFor + 500*time.Millisecond)
	var resp Response
	for i := 0; i < 40; i++ {
		resp, err = s.Sign(ctx, wdCN, Request{ToAddress: dest, AssetID: nativeID, Amount: "1000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:c"})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
		g.br = breaker{now: time.Now}
	}
	if err != nil || resp.Nonce != 2 {
		t.Fatalf("retry after restart: nonce %d err %v, want nonce 2", resp.Nonce, err)
	}

	// 5. the audit chain the gateway wrote verifies (read-only container over the same volume)
	if out := docker(t, "", append(append([]string{"run", "--rm", "--user", "10001:10001"}, mounts...), image, "audit-verify")...); !strings.Contains(out, "audit chain OK") {
		t.Fatalf("audit-verify: %s", out)
	}
}

func mustDER(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("bad pem")
	}
	return blk.Bytes
}
