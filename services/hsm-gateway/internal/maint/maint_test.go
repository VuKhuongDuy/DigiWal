package maint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mw")
	now := time.Now()
	if open, _ := Status(p, now); open {
		t.Fatal("open without a file")
	}
	if _, err := Open(p, 2*time.Hour, now); err == nil {
		t.Fatal("a 2h window was accepted")
	}
	until, err := Open(p, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if open, u := Status(p, now.Add(14*time.Minute)); !open || !u.Equal(until) {
		t.Fatalf("not open inside the window: %v %v", open, u)
	}
	if open, _ := Status(p, now.Add(16*time.Minute)); open {
		t.Fatal("still open after it ended")
	}
	// a file that claims a 10-hour window is capped at an hour
	_ = os.WriteFile(p, []byte(`{"opened_at":"`+now.UTC().Format(time.RFC3339)+`","until":"`+now.Add(10*time.Hour).UTC().Format(time.RFC3339)+`"}`), 0o600)
	if open, _ := Status(p, now.Add(2*time.Hour)); open {
		t.Fatal("the cap was not applied")
	}
	_ = os.WriteFile(p, []byte("garbage"), 0o600)
	if open, _ := Status(p, now); open {
		t.Fatal("garbage counted as open")
	}
	if err := Close(p); err != nil || Close(p) != nil {
		t.Fatal(err)
	}
}

func TestOpenFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w")
	now := time.Unix(1_700_000_000, 0)
	if d := OpenFor(path, now); d != 0 {
		t.Fatalf("no window: %s", d)
	}
	if _, err := Open(path, 15*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if d := OpenFor(path, now.Add(7*time.Minute)); d != 7*time.Minute {
		t.Fatalf("open for %s, want 7m", d)
	}
	if d := OpenFor(path, now.Add(16*time.Minute)); d != 0 {
		t.Fatalf("expired window must report 0, got %s", d)
	}
}
