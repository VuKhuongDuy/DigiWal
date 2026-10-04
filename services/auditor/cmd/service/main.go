// Command auditor re-checks, after the fact, what the money services decided at write time
// (docs/design/reconciliation-audit.md). It is a DEV-ENVIRONMENT tool with read access to every store;
// it refuses to start unless AUDITOR_ENV=dev and the chain is on the dev allow-list. It writes only to
// its own database and raises findings, never repairs. This skeleton runs the scheduler and records
// every Audit Run in job_runs; the rule sets (W, D, L, T, C) register their jobs in later changes.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"digiwal/services/auditor/internal/audit"
	"digiwal/services/auditor/internal/guard"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"

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

	var jobs []job.Job // A4-A7 register theirs here
	if j, err := withdrawalAudit(ctx, st, rpc); err != nil {
		log.Fatal(err)
	} else if j != nil {
		jobs = append(jobs, j)
	}
	log.Printf("auditor running (dev): chain %d, %d job(s)", chainID, len(jobs))
	job.Run(ctx, st, jobs)
	if len(jobs) == 0 {
		<-ctx.Done()
	}
}

// withdrawalAudit wires rules W1-W10 to the stores it reads, as the read-only auditor_ro login
// (roles/). It is skipped, with a log line, unless all four connection strings are set.
func withdrawalAudit(ctx context.Context, st *store.Store, rpc *ethclient.Client) (job.Job, error) {
	dsns := map[string]string{
		"LEDGER_RO_DSN": os.Getenv("LEDGER_RO_DSN"), "WALLET_RO_DSN": os.Getenv("WALLET_RO_DSN"),
		"WITHDRAWAL_RO_DSN": os.Getenv("WITHDRAWAL_RO_DSN"), "HISTORY_RO_MONGO_URI": os.Getenv("HISTORY_RO_MONGO_URI"),
	}
	for k, v := range dsns {
		if v == "" {
			log.Printf("withdrawal_audit disabled: %s is not set", k)
			return nil, nil
		}
	}
	led, err := source.OpenPG(ctx, "ledger", dsns["LEDGER_RO_DSN"])
	if err != nil {
		return nil, err
	}
	wal, err := source.OpenPG(ctx, "wallet", dsns["WALLET_RO_DSN"])
	if err != nil {
		return nil, err
	}
	wd, err := source.OpenPG(ctx, "withdrawal", dsns["WITHDRAWAL_RO_DSN"])
	if err != nil {
		return nil, err
	}
	hist, err := source.OpenMongo(ctx, dsns["HISTORY_RO_MONGO_URI"], envOr("HISTORY_MONGO_DATABASE", "history"))
	if err != nil {
		return nil, err
	}
	every := envDuration("AUDIT_INTERVAL", 5*time.Minute)
	return &audit.WithdrawalAudit{
		Src:   &source.Withdrawals{Withdrawal: wd, Ledger: led, Wallet: wal, History: hist, Chain: source.EthChain{C: rpc}},
		Store: st, Every: every,
		Grace:    envDuration("AUDIT_GRACE", 2*every), // Objectives: Grace Period = 2 runs
		Escalate: envDuration("AUDIT_ESCALATE", 3*every),
	}, nil
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
