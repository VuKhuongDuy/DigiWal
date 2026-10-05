package sweep

// Observer is told what the sweeper does, for metrics (monitoring-dashboard.md section 5). sweeper keeps no
// state, so these are process metrics that restart with it: acceptable because in-flight work is short-lived and a
// restart re-consumes Kafka. It is optional (nil observes nothing) and only ever receives facts.
type Observer interface {
	// Start: a Hot-Wallet Gas Top-Up (kind "gas_topup") or a Sweep (kind "sweep") is now in flight. The returned
	// function ends it; ok says whether it was mined.
	Start(kind string) (done func(ok bool))
	// FeeBump: a Fee Bump of a stuck Gas Top-Up was signed and sent.
	FeeBump(kind string)
	// Alerted: an ALERT was raised: bump_exhausted, bump_refused, cancelled_by_operator or gas_fee_report_failed.
	Alerted(kind string)
	// Abandoned: a record was given up on (it can never succeed, or it failed MAX_ATTEMPTS times).
	Abandoned()
}

type nopObserver struct{}

func (nopObserver) Start(string) func(bool) { return func(bool) {} }
func (nopObserver) FeeBump(string)          {}
func (nopObserver) Alerted(string)          {}
func (nopObserver) Abandoned()              {}

func orNop(o Observer) Observer {
	if o == nil {
		return nopObserver{}
	}
	return o
}
