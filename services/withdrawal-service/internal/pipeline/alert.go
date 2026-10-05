package pipeline

import (
	"fmt"
	"log"
	"time"

	"digiwal/services/withdrawal-service/internal/store"
)

// alerter reports a request an operator must look at, at most once per `every` per (request, kind),
// so a stuck request does not repeat itself on every pass. The sink defaults to the log, prefixed
// "ALERT:". Not safe for concurrent use: each Pipeline and Reaper loop owns one.
type alerter struct{ seen map[string]time.Time }

// raise reports whether the ALERT was actually emitted (false: de-duplicated).
func (a *alerter) raise(now time.Time, every time.Duration, sink func(string), w store.Withdrawal, kind, msg string) bool {
	if every <= 0 {
		every = 10 * time.Minute
	}
	k := w.ID + "/" + kind
	if a.seen == nil {
		a.seen = map[string]time.Time{}
	}
	if last, ok := a.seen[k]; ok && now.Sub(last) < every {
		return false
	}
	a.seen[k] = now
	m := fmt.Sprintf("withdrawal %s: %s", w.ID, msg)
	if sink != nil {
		sink(m)
		return true
	}
	log.Print("ALERT: " + m)
	return true
}
