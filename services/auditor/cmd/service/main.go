// Command auditor re-checks, after the fact, what the money services decided at write time
// (docs/design/reconciliation-audit.md). It is a DEV-ENVIRONMENT tool with read access to every store;
// it refuses to start unless AUDITOR_ENV=dev and the chain is on the dev allow-list. It writes only to
// its own database and raises findings, never repairs. This skeleton runs the scheduler and records
// every Audit Run in job_runs; the rule sets (W, D, L, T, C) register their jobs in later changes.
package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"digiwal/services/auditor/internal/audit"
	"digiwal/services/auditor/internal/guard"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/treasury"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
	"digiwal/services/auditor/internal/walletclient"

	"github.com/ethereum/go-ethereum/ethclient"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	allowed, err := guard.ParseChainIDs(envOr("AUDITOR_ALLOWED_CHAIN_IDS", "31337"))
	if err != nil {
		log.Fatal(err)
	}
	// A non-dev deployment must fail before it opens a single connection.
	if os.Getenv("AUDITOR_ENV") != "dev" {
		log.Fatalf("refusing to start: AUDITOR_ENV=%q, the auditor is a dev-environment tool and needs AUDITOR_ENV=dev", os.Getenv("AUDITOR_ENV"))
	}

	rpc, err := ethclient.DialContext(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatalf("dialing RPC: %v", err)
	}
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		log.Fatalf("reading chain id: %v", err)
	}
	if err := guard.Check(os.Getenv("AUDITOR_ENV"), chainID.Int64(), allowed); err != nil {
		log.Fatal(err)
	}

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
		addr := ":" + envOr("PORT", "8080")
		log.Printf("healthz on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatal(err)
		}
	}()

	jobs, err := audits(ctx, st, rpc, chainID.Int64()) //
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("auditor running (dev): chain %d, %d job(s)", chainID, len(jobs))
	job.Run(ctx, st, jobs)
	if len(jobs) == 0 {
		<-ctx.Done()
	}
}

// audits wires the rule sets to the stores they read, as the read-only auditor_ro login (roles/). Each job
// is skipped, with a log line, unless every connection string and address it needs is set.
func audits(ctx context.Context, st *store.Store, rpc *ethclient.Client, chainID int64) ([]job.Job, error) {
	pgs := map[string]*source.PG{}
	pg := func(name, envKey string) (*source.PG, error) {
		if p, ok := pgs[name]; ok {
			return p, nil
		}
		dsn := os.Getenv(envKey)
		if dsn == "" {
			log.Printf("  %s is not set", envKey)
			return nil, nil
		}
		p, err := source.OpenPG(ctx, name, dsn)
		if err != nil {
			return nil, err
		}
		pgs[name] = p
		return p, nil
	}
	var hist *source.Mongo
	mongoDB := func() (*source.Mongo, error) {
		if hist != nil {
			return hist, nil
		}
		uri := os.Getenv("HISTORY_RO_MONGO_URI")
		if uri == "" {
			log.Printf("  HISTORY_RO_MONGO_URI is not set")
			return nil, nil
		}
		var err error
		hist, err = source.OpenMongo(ctx, uri, envOr("HISTORY_MONGO_DATABASE", "history"))
		return hist, err
	}

	every := envDuration("AUDIT_INTERVAL", 5*time.Minute)
	grace := envDuration("AUDIT_GRACE", 2*every) // Objectives: Grace Period = 2 runs
	escalate := envDuration("AUDIT_ESCALATE", 3*every)
	start, err := strconv.ParseInt(envOr("AUDIT_SCAN_START_BLOCK", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("AUDIT_SCAN_START_BLOCK: %w", err)
	}
	chain := source.EthChain{C: rpc}

	var jobs []job.Job
	skip := func(name string) { log.Printf("%s disabled (see above)", name) }

	led, err := pg("ledger", "LEDGER_RO_DSN")
	if err != nil {
		return nil, err
	}
	wal, err := pg("wallet", "WALLET_RO_DSN")
	if err != nil {
		return nil, err
	}
	wd, err := pg("withdrawal", "WITHDRAWAL_RO_DSN")
	if err != nil {
		return nil, err
	}
	cw, err := pg("chain-watcher", "WATCHER_RO_DSN")
	if err != nil {
		return nil, err
	}
	h, err := mongoDB()
	if err != nil {
		return nil, err
	}

	if led != nil {
		jobs = append(jobs, &audit.LedgerIntegrity{Src: &source.LedgerChecks{Ledger: led}, Store: st, Every: every})
	} else {
		skip("ledger_integrity")
	}
	if led != nil && wal != nil && wd != nil && h != nil {
		jobs = append(jobs, &audit.WithdrawalAudit{
			Src:   &source.Withdrawals{Withdrawal: wd, Ledger: led, Wallet: wal, History: h, Chain: chain},
			Store: st, Every: every, Grace: grace, Escalate: escalate,
		})
	} else {
		skip("withdrawal_audit")
	}
	if led != nil && wal != nil && cw != nil && h != nil {
		jobs = append(jobs, &audit.DepositAudit{
			Src:   &source.Deposits{Watcher: cw, Ledger: led, Wallet: wal, History: h, Chain: chain},
			Store: st, ChainID: chainID, Every: every, Grace: grace, Escalate: escalate, StartBlock: start,
		})
	} else {
		skip("deposit_audit")
	}
	tiers, err := tierAddresses(ctx, chainID)
	if err != nil {
		return nil, err
	}
	hot, warm := tiers.Hot, tiers.Warm
	if led != nil && wal != nil && wd != nil && hot != "" && warm != "" {
		jobs = append(jobs, &audit.TreasuryAudit{
			Src:   &source.Treasury{Wallet: wal, Ledger: led, Withdrawal: wd, Chain: chain, ChainID: chainID},
			Store: st, ChainID: chainID, Every: every, Grace: grace, StartBlock: start,
			Tiers: tiers,
		})
	} else {
		log.Printf("  no tier addresses: set AUDIT_WALLET_URL (and the TLS files), or AUDIT_HOT_ADDRESS and AUDIT_WARM_ADDRESS")
		skip("treasury_audit")
	}
	if led != nil && wal != nil && wd != nil && cw != nil && hot != "" && warm != "" {
		capital, err := envAmounts("RECON_CAPITAL")
		if err != nil {
			return nil, err
		}
		tolerance, err := envAmounts("RECON_TOLERANCE")
		if err != nil {
			return nil, err
		}
		confirms, err := strconv.ParseInt(envOr("RECON_CONFIRMS", "1"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("RECON_CONFIRMS: %w", err)
		}
		jobs = append(jobs, &audit.Reconciliation{
			Src:   &source.Recon{Ledger: led, Watcher: cw, Withdrawal: wd, Wallet: wal, Chain: chain, TxChain: chain, ChainID: chainID},
			Store: st, Every: every, Confirms: confirms, Capital: capital, Tolerance: tolerance,
			Tiers: tiers,
		})
	} else {
		skip("reconciliation")
	}
	return jobs, nil
}

// tierAddresses are the Hot, Warm and Cold addresses. They come from wallet-service
// (GET /v1/wallet/system-addresses over mTLS as CN=auditor) when AUDIT_WALLET_URL is set; AUDIT_HOT_ADDRESS,
// AUDIT_WARM_ADDRESS and AUDIT_COLD_ADDRESS, when set, take precedence per address. Either way the chain
// wallet-service reports must be the one the RPC serves.
func tierAddresses(ctx context.Context, rpcChainID int64) (treasury.Addresses, error) {
	var t treasury.Addresses
	if url := os.Getenv("AUDIT_WALLET_URL"); url != "" {
		cfg, err := walletclient.TLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			return t, err
		}
		c := &walletclient.Client{BaseURL: url, HTTP: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}}
		a, err := c.Fetch(ctx, 30, 4*time.Second, log.Printf)
		if err != nil {
			return t, fmt.Errorf("treasury addresses from wallet-service: %w", err)
		}
		if a.ChainID != rpcChainID {
			return t, fmt.Errorf("wallet-service runs chain %d but the RPC serves chain %d", a.ChainID, rpcChainID)
		}
		t = treasury.Addresses{Hot: a.Hot, Warm: a.Warm, Cold: a.Cold}
		log.Printf("tier addresses from wallet-service: hot %s, warm %s, cold %q", t.Hot, t.Warm, t.Cold)
	}
	for _, o := range []struct {
		env string
		dst *string
	}{{"AUDIT_HOT_ADDRESS", &t.Hot}, {"AUDIT_WARM_ADDRESS", &t.Warm}, {"AUDIT_COLD_ADDRESS", &t.Cold}} {
		if v := strings.ToLower(os.Getenv(o.env)); v != "" {
			*o.dst = v
		}
	}
	return t, nil
}

// envAmounts parses "SYMBOL=minor units,SYMBOL=minor units", e.g. RECON_CAPITAL=ETH=10000000000000000000.
func envAmounts(key string) (map[string]*big.Int, error) {
	out := map[string]*big.Int{}
	for _, part := range strings.Split(os.Getenv(key), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sym, val, ok := strings.Cut(part, "=")
		n, good := new(big.Int).SetString(strings.TrimSpace(val), 10)
		if !ok || !good || n.Sign() < 0 {
			return nil, fmt.Errorf("%s: %q is not SYMBOL=amount in minor units", key, part)
		}
		out[strings.TrimSpace(sym)] = n
	}
	return out, nil
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return d
}

func requireEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
