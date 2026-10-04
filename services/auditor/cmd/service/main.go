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

	jobs, err := audits(ctx, st, rpc, chainID.Int64()) // A7 registers its job there too
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
	hot, warm := strings.ToLower(os.Getenv("AUDIT_HOT_ADDRESS")), strings.ToLower(os.Getenv("AUDIT_WARM_ADDRESS"))
	if led != nil && wal != nil && wd != nil && hot != "" && warm != "" {
		jobs = append(jobs, &audit.TreasuryAudit{
			Src:   &source.Treasury{Wallet: wal, Ledger: led, Withdrawal: wd, Chain: chain, ChainID: chainID},
			Store: st, ChainID: chainID, Every: every, Grace: grace, StartBlock: start,
			Tiers: treasury.Addresses{Hot: hot, Warm: warm, Cold: strings.ToLower(os.Getenv("AUDIT_COLD_ADDRESS"))},
		})
	} else {
		log.Printf("  AUDIT_HOT_ADDRESS / AUDIT_WARM_ADDRESS must be set for the treasury audit")
		skip("treasury_audit")
	}
	return jobs, nil
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
