// hsm-gateway fronts the HSM Token over mTLS: it signs Hot transactions from Sign Intents under the
// Signing Policy and writes the hash-chained Audit Log (docs/design/key-custody-hsm.md).
//
//	hsm-gateway [serve]            run the service (default)
//	hsm-gateway init-token         one-off: initialise the token and generate the Hot Key and the attestation key inside it
//	hsm-gateway init-attest-key    one-off, for a token from before M3a: generate the attestation key inside it
//	hsm-gateway init-kek           one-off, for a token from before M3c: generate the vault KEK (kek-v1) inside it
//	hsm-gateway maintenance open DURATION | close | status   the Maintenance Window the KEK administration calls need (host access)
//	hsm-gateway audit-verify ...   verify the audit chain (and optionally an expected head)
//	hsm-gateway fingerprint FILE   print the SHA-256 fingerprint of a certificate, for the callers file
package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/gateway"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/maint"
	"digiwal/services/hsm-gateway/internal/policy"
	"digiwal/services/hsm-gateway/internal/server"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	exitFatal = 1
	exitHSM   = 3 // wrong PIN / PIN guard tripped: a supervisor must NOT keep retrying
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		serve()
	case "init-token":
		initToken()
	case "init-attest-key":
		initAttestKey()
	case "init-kek":
		initKEK()
	case "maintenance":
		maintenance(args)
	case "audit-verify":
		auditVerify(args)
	case "fingerprint":
		fingerprint(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func requireEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func hsmConfig() hsm.Config {
	pool, _ := strconv.Atoi(envOr("HSM_POOL_SIZE", "4"))
	return hsm.Config{
		Module:         envOr("PKCS11_MODULE", "/usr/lib/softhsm/libsofthsm2.so"),
		TokenLabel:     envOr("HSM_TOKEN_LABEL", "digiwal-hot"),
		KeyLabel:       envOr("HSM_KEY_LABEL", "hot-key"),
		AttestKeyLabel: envOr("HSM_ATTEST_KEY_LABEL", hsm.DefaultAttestKeyLabel),
		PINFile:        envOr("HSM_PIN_FILE", "/run/secrets/hsm-user-pin"),
		PoolSize:       pool,
		Guard:          hsm.NewPINGuard(envOr("PIN_GUARD_FILE", "/var/lib/hsm-gateway/state/pin-guard.json"), 3, time.Hour),
	}
}

func serve() {
	polPath := requireEnv("POLICY_FILE")
	pol, err := policy.LoadFile(polPath, policy.LoadOptions{RequireRootOwner: envOr("POLICY_REQUIRE_ROOT_OWNER", "true") != "false"})
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("signing policy loaded file=%s sha256=%s enabled=%v chain_id=%d", polPath, pol.Hash, pol.Enabled, pol.ChainID)

	pins, err := authz.LoadPins(requireEnv("CALLERS_FILE"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	tlsCfg, err := authz.ServerTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"), pins)
	if err != nil {
		log.Fatalf("configuring mTLS: %v", err)
	}

	alog, opened, err := audit.Open(envOr("AUDIT_LOG_FILE", "/var/lib/hsm-gateway/audit/audit.log"))
	if err != nil {
		log.Fatalf("audit log: %v", err)
	}
	if opened.TrimmedTorn {
		log.Printf("WARNING audit log: removed an incomplete final line (a crash mid-append; that request never completed)")
	}
	seq, head := alog.Head()
	log.Printf("audit log verified entries=%d head_seq=%d head_hash=%s", len(opened.Entries), seq, head)

	tok, err := hsm.Open(hsmConfig())
	switch {
	case errors.Is(err, hsm.ErrPINIncorrect), errors.Is(err, hsm.ErrPINGuardTripped):
		log.Printf("ALERT HSM login refused, exiting without retry: %v", err)
		os.Exit(exitHSM)
	case err != nil:
		log.Fatalf("opening the HSM token: %v", err)
	}
	defer tok.Close()

	reg := metrics.NewRegistry()
	svc, err := gateway.New(pol, alog, opened.Entries, tok, reg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if tok.HasAttestKey() {
		svc.WithAttestor(tok)
		log.Printf("attestation key present (P-256), POST /v1/attest enabled, public key %x", tok.AttestPublicKey())
	} else {
		log.Printf("WARNING the token has no attestation key: POST /v1/attest answers 503 (run init-attest-key)")
	}
	mwFile := maintFile()
	if _, err := tok.KEKs(context.Background()); err != nil {
		log.Fatalf("a KEK in the token fails its attribute checks, refusing to serve: %v", err)
	}
	// 0 while closed, else seconds since it opened: alerts.yml HsmMaintenanceWindowLong (a window is meant to last minutes).
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "digiwal_hsm_gateway_maintenance_window_open_seconds",
		Help: "Seconds the Maintenance Window has been open (0 when closed)."},
		func() float64 { return maint.OpenFor(mwFile, time.Now()).Seconds() }))
	svc.WithVault(tok, gateway.VaultOptions{
		UnsealPerMinute: func() int { n, _ := strconv.Atoi(envOr("VAULT_UNSEAL_PER_MIN", "60")); return n }(),
		Window:          func(now time.Time) (bool, time.Time) { return maint.Status(mwFile, now) },
		Registry:        reg,
	})
	if svc.VaultEnabled() {
		ks, _ := tok.KEKs(context.Background())
		log.Printf("vault enabled: KEKs %v, POST /v1/vault/{seal,unseal,verify,reseal} and /v1/vault/kek/* (unseal: sweep-signer only, %s/min cap)", ks, envOr("VAULT_UNSEAL_PER_MIN", "60"))
	} else {
		log.Printf("WARNING the token has no KEK: /v1/vault/* answers 503 (run init-kek)")
	}
	if err := svc.SelfTest(context.Background()); err != nil {
		log.Fatalf("startup self-test failed, refusing to serve: %v", err)
	}
	log.Printf("startup self-test passed hot_address=%s", svc.Hot())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()
	interval, _ := time.ParseDuration(envOr("SELFTEST_INTERVAL", "10m"))
	go svc.RunSelfTests(ctx, interval, func(err error) {
		var he *hsm.Error
		if errors.As(err, &he) && he.Code == hsm.CodeNotLoggedIn {
			log.Printf("ALERT the HSM login is gone: exiting so the next start re-logs in through the PIN guard")
			os.Exit(2)
		}
		if cur, ferr := policy.LoadFile(polPath, policy.LoadOptions{}); ferr == nil && cur.Hash != pol.Hash {
			log.Printf("WARNING the policy file on disk (sha256=%s) differs from the enforced one (%s): restart to apply", cur.Hash, pol.Hash)
		}
	})

	srv := server.New(svc, pins)
	mtls := &http.Server{Addr: envOr("MTLS_ADDR", ":8443"), Handler: srv.MTLSRoutes(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		log.Printf("hsm-gateway mTLS listening on %s", mtls.Addr)
		if err := mtls.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("mTLS listener: %v", err)
		}
	}()
	plain := &http.Server{Addr: ":" + envOr("PORT", "8080"), Handler: srv.PlainRoutes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := plain.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("plain listener: %v", err)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mtls.Shutdown(sctx)
	_ = plain.Shutdown(sctx)
}

func initToken() {
	cfg := hsmConfig()
	fmt.Fprintln(os.Stderr, "SO PIN (one line on stdin; it is used once and never stored):")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		log.Fatalf("reading the SO PIN from stdin: %v", err)
	}
	so := trimNL(line)
	res, err := hsm.InitTokenAndKey(cfg, so)
	if err != nil {
		log.Fatalf("init-token: %v", err)
	}
	fmt.Printf("token_initialised=%v\nhot_address=%s\nhot_public_key=0x%x\n", res.NewToken, res.Address, res.PublicKey)
	fmt.Printf("attest_public_key=%x\n", res.AttestPublicKey)
	fmt.Printf("kek_version=%d\n", res.KEKVersion)
	fmt.Println("next: put hot_address in the policy file; pin attest_public_key in wallet-service's XPUB_ATTEST_PUBKEY_FILE (outside the database); the keys never leave the token")
}

func initKEK() {
	v, err := hsm.InitKEK(hsmConfig())
	if err != nil {
		log.Fatalf("init-kek: %v", err)
	}
	fmt.Printf("kek_version=%d\n", v)
	fmt.Println("next: restart hsm-gateway so it serves /v1/vault/*; the KEK never leaves the token (AES-256, private, sensitive, non-extractable)")
}

func maintFile() string {
	return envOr("MAINTENANCE_FILE", "/var/lib/hsm-gateway/state/maintenance-window")
}

// maintenance opens or closes the Maintenance Window that KEK administration needs. It writes a file in the gateway's state directory, so it
// needs the same host access as the gateway itself.
func maintenance(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hsm-gateway maintenance open DURATION | close | status")
		os.Exit(2)
	}
	switch args[0] {
	case "open":
		d := 15 * time.Minute
		if len(args) > 1 {
			var err error
			if d, err = time.ParseDuration(args[1]); err != nil {
				log.Fatalf("duration: %v", err)
			}
		}
		until, err := maint.Open(maintFile(), d, time.Now())
		if err != nil {
			log.Fatalf("maintenance open: %v", err)
		}
		fmt.Printf("maintenance window open until %s\n", until.UTC().Format(time.RFC3339))
	case "close":
		if err := maint.Close(maintFile()); err != nil {
			log.Fatalf("maintenance close: %v", err)
		}
		fmt.Println("maintenance window closed")
	case "status":
		open, until := maint.Status(maintFile(), time.Now())
		fmt.Printf("maintenance_window_open=%v until=%s\n", open, until.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintln(os.Stderr, "usage: hsm-gateway maintenance open DURATION | close | status")
		os.Exit(2)
	}
}

func initAttestKey() {
	pk, err := hsm.InitAttestKey(hsmConfig())
	if err != nil {
		log.Fatalf("init-attest-key: %v", err)
	}
	fmt.Printf("attest_public_key=%x\n", pk)
	fmt.Println("next: pin attest_public_key in wallet-service's XPUB_ATTEST_PUBKEY_FILE (outside the database); the key never leaves the token")
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func auditVerify(args []string) {
	fs := flag.NewFlagSet("audit-verify", flag.ExitOnError)
	file := fs.String("file", envOr("AUDIT_LOG_FILE", "/var/lib/hsm-gateway/audit/audit.log"), "audit log path")
	seq := fs.Int64("expect-seq", -1, "expected head sequence (from the auditor's recorded head); detects tail truncation")
	hash := fs.String("expect-hash", "", "expected head hash")
	_ = fs.Parse(args)
	var o audit.VerifyOptions
	if *seq >= 0 {
		v := uint64(*seq)
		o.ExpectSeq = &v
	}
	o.ExpectHash = *hash
	s, h, err := audit.Verify(*file, o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "AUDIT VERIFY FAILED: %v\n", err)
		os.Exit(exitFatal)
	}
	fmt.Printf("audit chain OK entries=%d head_seq=%d head_hash=%s\n", s, s, h)
}

func fingerprint(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hsm-gateway fingerprint cert.pem")
		os.Exit(2)
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		log.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		log.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("cn=%s sha256=%s\n", c.Subject.CommonName, authz.Fingerprint(c.Raw))
}
