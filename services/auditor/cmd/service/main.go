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

	"digiwal/services/auditor/internal/guard"
	"digiwal/services/auditor/internal/job"
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

	var jobs []job.Job // A3-A7 register theirs here
	log.Printf("auditor running (dev): chain %d, %d job(s)", chainID, len(jobs))
	job.Run(ctx, st, jobs)
	if len(jobs) == 0 {
		<-ctx.Done()
	}
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
