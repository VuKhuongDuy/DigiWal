package pipeline

import "time"

// Observer is told what the background loops do, for metrics (monitoring-dashboard.md section 5). It is optional
// (nil observes nothing), and it only ever receives facts: it can neither change nor block what the loops decide.
type Observer interface {
	// Reclaimed: the Reaper failed a stuck request. rule is the failure reason it recorded
	// (stale_request, stale_reservation, replaced_or_dropped).
	Reclaimed(rule string)
	// Alerted: an ALERT was actually emitted (after de-duplication), of the given kind.
	Alerted(kind string)
	// ReaperPass: the Reaper finished a pass at this time, whatever its outcome.
	ReaperPass(at time.Time)
}

type nopObserver struct{}

func (nopObserver) Reclaimed(string)     {}
func (nopObserver) Alerted(string)       {}
func (nopObserver) ReaperPass(time.Time) {}

func orNop(o Observer) Observer {
	if o == nil {
		return nopObserver{}
	}
	return o
}
