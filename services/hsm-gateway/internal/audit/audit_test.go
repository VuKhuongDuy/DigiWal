package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func write(t *testing.T, path string, n int) *Log {
	t.Helper()
	l, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := l.Append(Entry{Op: OpHotSign, CallerCN: "wallet-service", Nonce: uint64(i), Result: ResultAuthorized, Rule: "accept_payout", PolicyHash: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestChain_AppendVerifyReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "audit.log")
	l := write(t, path, 5)
	seq, head := l.Head()
	if seq != 5 {
		t.Fatalf("seq %d", seq)
	}
	_ = l.Close()
	s, h, err := Verify(path, VerifyOptions{ExpectSeq: &seq, ExpectHash: head})
	if err != nil || s != 5 || h != head {
		t.Fatalf("verify: %d %s %v", s, h, err)
	}
	// reopen continues the chain
	l2, op, err := Open(path)
	if err != nil || len(op.Entries) != 5 {
		t.Fatalf("reopen: %v entries=%d", err, len(op.Entries))
	}
	if _, err := l2.Append(Entry{Op: OpHotSign, Result: ResultRefused}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(path, VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestVerify_DetectsTamperingAndRemoval(t *testing.T) {
	for name, edit := range map[string]func(ls []string) string{
		"modified field": func(ls []string) string {
			ls[2] = strings.Replace(ls[2], `"nonce":2`, `"nonce":9`, 1)
			return strings.Join(ls, "")
		},
		"entry deleted":    func(ls []string) string { return strings.Join(append(ls[:2:2], ls[3:]...), "") },
		"entries swapped":  func(ls []string) string { ls[1], ls[2] = ls[2], ls[1]; return strings.Join(ls, "") },
		"entry duplicated": func(ls []string) string { return strings.Join(append(ls[:3:3], ls[2:]...), "") },
		"hash field rewrite": func(ls []string) string {
			ls[3] = strings.Replace(ls[3], `"hash":"`, `"hash":"00`, 1)
			return strings.Join(ls, "")
		},
	} {
		path := filepath.Join(t.TempDir(), "audit.log")
		l := write(t, path, 6)
		_ = l.Close()
		ls := lines(t, path)
		for i := range ls {
			if !strings.HasSuffix(ls[i], "\n") {
				ls[i] += "\n"
			}
		}
		if err := os.WriteFile(path, []byte(edit(ls)), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Verify(path, VerifyOptions{}); err == nil {
			t.Errorf("%s: verify passed", name)
		}
		if _, _, err := Open(path); err == nil {
			t.Errorf("%s: Open accepted a broken chain", name)
		}
	}
}

// Truncating whole entries off the tail leaves a self-consistent chain; only the head recorded
// elsewhere (the auditor's) exposes it. Both outcomes are asserted.
func TestVerify_TailTruncationNeedsTheExpectedHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l := write(t, path, 6)
	seq, head := l.Head()
	_ = l.Close()
	ls := lines(t, path)
	trunc := strings.Join(ls[:4], "") // drop the last two entries
	if !strings.HasSuffix(trunc, "\n") {
		trunc += "\n"
	}
	if err := os.WriteFile(path, []byte(trunc), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(path, VerifyOptions{}); err != nil {
		t.Fatalf("a clean tail truncation is internally consistent, verify without a head should pass: %v", err)
	}
	if _, _, err := Verify(path, VerifyOptions{ExpectSeq: &seq}); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("verify against the recorded head seq must detect truncation, got %v", err)
	}
	if _, _, err := Verify(path, VerifyOptions{ExpectHash: head}); err == nil {
		t.Fatal("verify against the recorded head hash must detect truncation")
	}
}

func TestOpen_TornFinalLineIsTrimmedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l := write(t, path, 3)
	seq, head := l.Head()
	_ = l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"seq":4,"ts":"x","op":"hot_si`) // crash mid-append
	_ = f.Close()
	if _, _, err := Verify(path, VerifyOptions{}); err == nil {
		t.Fatal("verify must flag a torn final line")
	}
	l2, op, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !op.TrimmedTorn || len(op.Entries) != 3 {
		t.Fatalf("trimmed=%v entries=%d", op.TrimmedTorn, len(op.Entries))
	}
	if s, h := l2.Head(); s != seq || h != head {
		t.Fatal("head changed by trimming")
	}
	if _, err := l2.Append(Entry{Op: OpHotSign, Result: ResultRefused}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(path, VerifyOptions{}); err != nil {
		t.Fatalf("log not clean after trim+append: %v", err)
	}
}

func TestAppend_FailureIsStickyAndReturnsNoEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l := write(t, path, 1)
	_ = l.f.Close() // simulate a dead disk/handle
	if _, err := l.Append(Entry{Op: OpHotSign}); err == nil {
		t.Fatal("append on a dead file succeeded")
	}
	if l.Healthy() {
		t.Fatal("log reports healthy after a failed write")
	}
	if _, err := l.Append(Entry{Op: OpHotSign}); err == nil {
		t.Fatal("log kept accepting appends after a failed write")
	}
}

func TestAppend_ConcurrentWritersKeepOneChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, _, _ := Open(path)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := l.Append(Entry{Op: OpHotSign, Result: ResultAuthorized}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if s, _, err := Verify(path, VerifyOptions{}); err != nil || s != 200 {
		t.Fatalf("seq %d err %v", s, err)
	}
}
