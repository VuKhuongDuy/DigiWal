// Command key-admin is the operator's one-off ceremony tool (docs/design/key-custody-hsm.md: Ceremony, `key-admin`). It runs as a
// short-lived job with its own leaf (CN key-admin, pinned by hsm-gateway) and its own database login for the vault (svc_keyadmin).
// It never holds a master key: the user_derivation Seed is sealed by hsm-gateway under a KEK that lives in the HSM token.
//
//	key-admin init [-resume]
//	    Fresh initialisation (docs/runbooks/dev-hsm-bringup.md): generate the Seed, prove address parity, have hsm-gateway seal it,
//	    store the sealed row PENDING, have the gateway open it again and compare the xpub, show the mnemonic ONCE, have the gateway
//	    attest the xpub, store wallet_xpubs, re-prove parity against the stored xpub, then flip the row to ACTIVE. Refuses if a sealed
//	    secret already exists. -resume finishes an interrupted init from the pending row (no new seed).
//	key-admin migrate                        apply the custody database migrations (POSTGRES_DSN, owner login) and exit: the first step of a fresh bring-up
//	key-admin status                         print the rows and KEKs; exit 0 if initialised, 3 if not (scripts use it to make init idempotent)
//	key-admin rewrap [-kek N]                new PENDING row: the same DEK wrapped by kek-vN (default newest), re-encrypted inside the HSM, verified; no flip
//	key-admin activate                       flip the verified pending row
//	key-admin rotate-kek                     generate the next KEK if needed, rewrap, verify, flip
//	key-admin rotate-dek                     new DEK under the active KEK (the Seed is re-encrypted inside the HSM), verify, flip
//	key-admin rollback                       flip back to the newest retired row (its KEK must still exist)
//	key-admin retire-kek N                   make kek-vN unwrap-only (refused while a live row is wrapped under it)
//	key-admin destroy-kek [-force] N         delete kek-vN from the token (refused while a live row needs it)
//
// Every KEK administration and re-encryption call needs a Maintenance Window opened on the gateway host first
// (`hsm-gateway maintenance open 15m`). Rotating wrappers does NOT help after a Seed leak: that is the Seed rotation runbook
// (docs/runbooks/seed-rotation.md).
//
// Environment: POSTGRES_DSN (custody database, owner login: wallet_addresses, wallet_xpubs), VAULT_DSN + VAULT_DB_PASSWORD_FILE (role svc_keyadmin,
// the password in a secret FILE), HSM_GATEWAY_URL, TLS_CERT_FILE / TLS_KEY_FILE (the key-admin leaf), TLS_CA_FILE, XPUB_ATTEST_PUBKEY_FILE
// (the attestation public key pinned outside the database).
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"digiwal/services/wallet-service/internal/keyadmin"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/vaultstore"
	"digiwal/services/wallet-service/internal/xpub"
)

const usage = "usage: key-admin migrate | init [-resume] | status | rewrap [-kek N] | activate | rotate-kek | rotate-dek | rollback | retire-kek N | destroy-kek [-force] N"

// exitNotInitialised is `status`'s exit code for "no active secret".
const exitNotInitialised = 3

// legacy holds the pre-M3c commands that need the old software key; legacy.go fills it in a `!prod` build only.
var legacy = map[string]func(ctx context.Context, args []string){}

func main() {
	if len(os.Args) < 2 {
		log.Fatal(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	if f, ok := legacy[cmd]; ok {
		fmt.Fprintf(os.Stderr, "WARNING: key-admin %s is a LEGACY command (old software master key, !prod builds only); it is not part of the HSM flow\n", cmd)
		f(ctx, args)
		return
	}
	switch cmd {
	case "migrate":
		cs, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN")) // Connect runs the embedded migrations
		fatal(err)
		cs.Close()
		fmt.Println("custody migrations applied")
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		resume := fs.Bool("resume", false, "finish an interrupted init from the pending row")
		_ = fs.Parse(args)
		d, closeAll := deps(ctx, true)
		defer closeAll()
		fatal(keyadmin.FreshInit(ctx, d, *resume))
	case "status":
		d, closeAll := deps(ctx, false)
		defer closeAll()
		ok, err := keyadmin.Status(ctx, d)
		fatal(err)
		if !ok {
			closeAll()
			os.Exit(exitNotInitialised)
		}
	case "rewrap":
		fs := flag.NewFlagSet("rewrap", flag.ExitOnError)
		kek := fs.Int("kek", 0, "the KEK version to wrap under (default: the newest)")
		_ = fs.Parse(args)
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.Rotate(ctx, d, keyadmin.RotateOpts{ToKEK: *kek}))
	case "rotate-kek":
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.Rotate(ctx, d, keyadmin.RotateOpts{Flip: true, GenerateKEK: true}))
	case "rotate-dek":
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.Rotate(ctx, d, keyadmin.RotateOpts{Flip: true, NewDEK: true}))
	case "activate":
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.Activate(ctx, d))
	case "rollback":
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.Rollback(ctx, d))
	case "retire-kek":
		v := kekArg(args)
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.RetireKEK(ctx, d, v))
	case "destroy-kek":
		fs := flag.NewFlagSet("destroy-kek", flag.ExitOnError)
		force := fs.Bool("force", false, "destroy even if a retired row (the rollback target) is wrapped under it")
		_ = fs.Parse(args)
		v := kekArg(fs.Args())
		d, closeAll := deps(ctx, false)
		defer closeAll()
		fatal(keyadmin.DestroyKEK(ctx, d, v, *force))
	default:
		log.Fatal(usage)
	}
}

func fatal(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func kekArg(args []string) int {
	if len(args) != 1 {
		log.Fatal(usage)
	}
	v, err := strconv.Atoi(args[0])
	if err != nil || v < 1 {
		log.Fatalf("not a KEK version: %q", args[0])
	}
	return v
}

// deps connects everything the ceremonies need. custody is true for init (it also writes wallet_xpubs through the owner login).
func deps(ctx context.Context, custody bool) (keyadmin.Deps, func()) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		closers = nil
	}
	vs, err := vaultstore.Connect(ctx, requireEnv("VAULT_DSN"), os.Getenv("VAULT_DB_PASSWORD_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	closers = append(closers, vs.Close)
	cs, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	closers = append(closers, cs.Close)

	var pin *ecdsa.PublicKey
	if p := os.Getenv("XPUB_ATTEST_PUBKEY_FILE"); p != "" {
		if pin, err = xpub.LoadPinned(p); err != nil {
			log.Fatalf("XPUB_ATTEST_PUBKEY_FILE: %v", err)
		}
	} else if custody {
		log.Fatal("XPUB_ATTEST_PUBKEY_FILE is required: the attestation key is pinned outside the database")
	}
	tlsCfg, err := signing.GatewayTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("hsm-gateway client TLS: %v", err)
	}
	g := keyadmin.NewGatewayClient(requireEnv("HSM_GATEWAY_URL"), tlsCfg)
	// The gateway may still be starting (compose brings it up in parallel): wait for it, once, up front.
	wait := 60 * time.Second
	deadline := time.Now().Add(wait)
	for {
		if _, err := g.KEKs(ctx); err == nil {
			break
		} else if time.Now().After(deadline) || strings.Contains(err.Error(), "vault_unavailable") {
			log.Fatalf("hsm-gateway did not answer GET /v1/vault/keks within %s: %v (a token from before M3c needs `hsm-gateway init-kek` and a restart)", wait, err)
		}
		time.Sleep(time.Second)
	}
	return keyadmin.Deps{Vault: vs, GW: g, Attest: g, Custody: cs, XStore: cs, Pin: pin, Out: os.Stdout}, closeAll
}

func requireEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required env var %s is not set\n", k)
		os.Exit(2)
	}
	return strings.TrimSpace(v)
}
