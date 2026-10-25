//go:build !prod

// Command hot-cutover is the operator tool for the Hot cutover of docs/design/key-custody-hsm.md 5.5 (M2c): it moves
// the Hot Wallet's funds from the old software Hot address to the new HSM-held one, with the old software key, and
// switches nothing else by itself. It is the ONE place that still opens the legacy hot_wallet seed after the HSM
// backend exists, which is why it is a separate binary behind a `!prod` build tag and its own image stage
// (`--target cutover`): wallet-service's production image never contains it. There is no network surface; the
// freeze is a database row only this tool moves. Runbook: docs/runbooks/hot-cutover.md.
//
//	hot-cutover [flags] preflight | freeze | settle | drain | verify | complete -switched | abort
//
// Every command is idempotent. -dry-run prints what it would do and changes nothing.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/cutover"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/treasury"
)

const usage = "usage: hot-cutover [-dry-run] -new <0x new Hot address> [-confirmations N] [-timeout 10m] preflight|freeze|settle|drain|verify|complete [-switched]|abort"

func main() {
	fs := flag.NewFlagSet("hot-cutover", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print what would be done and change nothing")
	newHot := fs.String("new", "", "the new Hot address exactly as the HSM ceremony printed it (checked against hsm-gateway GET /v1/hot/key)")
	conf := fs.Uint64("confirmations", 2, "blocks on top of a tx before it counts as confirmed (Withdrawal Confirmation Depth: 2 on the e2e chain, 64 on Sepolia)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long settle and drain wait for the chain")
	switched := fs.Bool("switched", false, "complete only: wallet-service now runs on SIGNER_BACKEND=pkcs11 and its consumers were restarted")
	_ = fs.Parse(os.Args[1:])
	if fs.NArg() < 1 {
		log.Fatal(usage)
	}
	cmd := fs.Arg(0)
	_ = fs.Parse(fs.Args()[1:]) // flags may follow the command too: `complete -switched`
	if fs.NArg() != 0 {
		log.Fatal(usage)
	}
	if !common.IsHexAddress(*newHot) {
		log.Fatalf("-new is required and must be a 20-byte hex address\n%s", usage)
	}
	switch cmd {
	case "preflight", "freeze", "settle", "drain", "verify", "complete", "abort":
	default:
		log.Fatalf("unknown command %q\n%s", cmd, usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	assets, err := signing.LoadAssets(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatalf("loading assets: %v", err)
	}
	rpc, err := cutover.DialRPC(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		log.Fatalf("reading the chain id: %v", err)
	}

	// The old key: the sealed hot_wallet seed at the Hot index. Needed by every command (it names the old address).
	sealer, err := crypto.NewSealer(requireEnv("MASTER_KEY_HEX"))
	if err != nil {
		log.Fatal(err)
	}
	sealed, err := store.SealedSeed(ctx, storage.RoleHotWallet)
	if err != nil {
		log.Fatalf("reading the hot_wallet seed (the old Hot key): %v", err)
	}
	mnemonic, err := sealer.Open(sealed)
	if err != nil {
		log.Fatalf("decrypting the hot_wallet seed (wrong MASTER_KEY_HEX?): %v", err)
	}
	w, err := hdwallet.FromMnemonic(string(mnemonic))
	if err != nil {
		log.Fatal(err)
	}
	legacy, err := cutover.NewLegacy(ctx, signing.SoftwareHot(w, hdwallet.HotWalletIndex))
	if err != nil {
		log.Fatal(err)
	}

	// The new key: hsm-gateway, as wallet-service itself would pin it (mTLS as CN=wallet-service, which the gateway pins).
	tlsCfg, err := signing.GatewayTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("hsm-gateway client TLS: %v", err)
	}
	gw, err := signing.NewGatewayHot(ctx, signing.GatewayConfig{BaseURL: requireEnv("HSM_GATEWAY_URL"), TLS: tlsCfg, CallerCN: "hot-cutover", StartRetry: 20 * time.Second})
	if err != nil {
		log.Fatalf("hsm-gateway is not reachable: %v (the HSM backend must be up before the cutover)", err)
	}

	// wallet-service's own start-up check, run here too: the new Hot address must not be the Warm address.
	tf, err := treasury.Load(requireEnv("TREASURY_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := tf.WarmFor(chainID.Int64(), common.HexToAddress(*newHot)); err != nil {
		log.Fatalf("treasury check for the new Hot address: %v", err)
	}

	var ledger cutover.Ledger
	if cmd == "drain" {
		hc, err := cutover.LedgerTLS(requireEnv("LEDGER_TLS_CERT_FILE"), requireEnv("LEDGER_TLS_KEY_FILE"), requireEnv("LEDGER_TLS_CA_FILE"))
		if err != nil {
			log.Fatal(err)
		}
		ledger = &cutover.LedgerClient{BaseURL: requireEnv("LEDGER_SERVICE_URL"), HTTP: hc}
	}

	ceiling := signing.DefaultFeeCeiling
	if v := os.Getenv("HOT_WALLET_MAX_FEE_PER_GAS_WEI"); v != "" {
		n, ok := new(big.Int).SetString(v, 10)
		if !ok || n.Sign() <= 0 {
			log.Fatalf("HOT_WALLET_MAX_FEE_PER_GAS_WEI=%q is not a positive integer (wei)", v)
		}
		ceiling = n
	}
	tool := cutover.New(cutover.Config{
		ChainID: chainID.Int64(), Assets: assets, NewHot: common.HexToAddress(*newHot), Confirmations: *conf,
		Timeout: *timeout, FeeCeiling: ceiling, DryRun: *dry, Out: os.Stdout,
	}, rpc, store, cutover.GatewayFromBackend{G: gw}, legacy, ledger)

	var runErr error
	switch cmd {
	case "preflight":
		runErr = tool.Preflight(ctx)
	case "freeze":
		runErr = tool.Freeze(ctx)
	case "settle":
		runErr = tool.Settle(ctx)
	case "drain":
		runErr = tool.Drain(ctx)
	case "verify":
		_, runErr = tool.Verify(ctx)
	case "complete":
		runErr = tool.Complete(ctx, *switched)
	case "abort":
		runErr = tool.Abort(ctx)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "hot-cutover %s: %v\n", cmd, runErr)
		os.Exit(1)
	}
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
