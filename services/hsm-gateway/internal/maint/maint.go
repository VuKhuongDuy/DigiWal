// Package maint is the Maintenance Window of hsm-gateway (key-custody-hsm.md invariant 25 and 5.4): the KEK administration operations
// (generate, retire, destroy, reseal for rewrap and DEK rotation) work only while a window is open. A window is a small file in the
// gateway's state directory written by the operator on the vm2 host (`hsm-gateway maintenance open 15m`), so it needs host access: a
// stolen key-admin certificate alone cannot open one. The gateway reads the file on every admin call; nothing is cached.
package maint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// MaxWindow is the longest window that is honoured, whatever the file says.
const MaxWindow = time.Hour

type window struct {
	OpenedAt time.Time `json:"opened_at"`
	Until    time.Time `json:"until"`
}

// Open writes a window of length d (capped at MaxWindow) to path, replacing any existing one.
func Open(path string, d time.Duration, now time.Time) (until time.Time, err error) {
	if d <= 0 || d > MaxWindow {
		return time.Time{}, fmt.Errorf("a maintenance window is between 1 second and %s", MaxWindow)
	}
	w := window{OpenedAt: now.UTC(), Until: now.UTC().Add(d)}
	b, _ := json.Marshal(w)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return time.Time{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return time.Time{}, err
	}
	return w.Until, nil
}

// Close removes the window (closing an already closed window is not an error).
func Close(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Status reports whether a window is open at now and when it ends. A missing, unreadable or malformed file is closed.
func Status(path string, now time.Time) (open bool, until time.Time) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, time.Time{}
	}
	var w window
	if json.Unmarshal(b, &w) != nil || w.Until.IsZero() || w.OpenedAt.IsZero() {
		return false, time.Time{}
	}
	if w.Until.After(w.OpenedAt.Add(MaxWindow)) {
		w.Until = w.OpenedAt.Add(MaxWindow)
	}
	if !now.Before(w.Until) || now.Before(w.OpenedAt.Add(-time.Minute)) {
		return false, w.Until
	}
	return true, w.Until
}

// OpenFor is how long the window has been open at now (0 when none is open). It feeds the "window open longer than expected" alert.
func OpenFor(path string, now time.Time) time.Duration {
	if open, _ := Status(path, now); !open {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var w window
	if json.Unmarshal(b, &w) != nil || now.Before(w.OpenedAt) {
		return 0
	}
	return now.Sub(w.OpenedAt)
}
