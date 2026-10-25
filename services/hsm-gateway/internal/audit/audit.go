// Package audit is the append-only, hash-chained Audit Log (key-custody-hsm.md invariant 8).
// Every sign request is appended and fsynced BEFORE the HSM is called and before anything is
// returned; if the append fails there is no signature. Each line carries the SHA-256 of the previous
// line's record, so edits and mid-file removals are detectable from the file alone, and tail
// truncation is detectable by comparing against a head (seq, hash) recorded elsewhere (the
// auditor polls GET /v1/audit/head from the other node).
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Results.
const (
	ResultAuthorized = "authorized" // policy passed; the HSM call follows
	ResultRefused    = "refused"
	ResultHSMError   = "hsm_error" // follows an "authorized" entry whose signing failed
)

// Operations.
const (
	OpHotSign  = "hot_sign"
	OpSelfTest = "self_test"
	OpAttest   = "attest" // an xpub Attestation request (M3a): Digest is the signed statement digest, Detail the xpub fingerprint
	// Vault operations (M3c): Class is "vault"; Detail names role, purpose, row version, KEK version and a short fingerprint of the
	// wrapped DEK (never the DEK, never the plaintext).
	OpVaultSeal   = "seal"
	OpVaultUnseal = "unseal"
	OpVaultVerify = "verify"
	OpVaultReseal = "reseal"
	OpKEKGenerate = "kek_generate"
	OpKEKRetire   = "kek_retire"
	OpKEKDestroy  = "kek_destroy"
)

// ZeroHash is the prev of the first entry.
var ZeroHash = hex.EncodeToString(make([]byte, 32))

// Entry is one log line. Field order is the hash input order (encoding/json is deterministic for
// structs); Hash is computed with Hash == "".
type Entry struct {
	Seq         uint64 `json:"seq"`
	TS          string `json:"ts"`
	Op          string `json:"op"`
	CallerCN    string `json:"caller_cn"`
	CallerClaim string `json:"caller_cn_claim,omitempty"`
	Idempotency string `json:"idempotency_key,omitempty"`
	ChainID     uint64 `json:"chain_id"`
	Nonce       uint64 `json:"nonce"`
	To          string `json:"to"`
	Value       string `json:"value"`
	Data        string `json:"data"`
	Gas         uint64 `json:"gas"`
	MaxFee      string `json:"max_fee_per_gas"`
	Tip         string `json:"max_priority_fee_per_gas"`
	Class       string `json:"class"`
	Asset       string `json:"asset,omitempty"`
	Amount      string `json:"amount,omitempty"`
	FeeWei      string `json:"fee_wei,omitempty"`
	Selector    string `json:"selector,omitempty"`
	Recipient   string `json:"recipient,omitempty"`
	Replacement bool   `json:"replacement,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Rule        string `json:"rule"`
	Result      string `json:"result"`
	Detail      string `json:"detail,omitempty"`
	PolicyHash  string `json:"policy_hash"`
	TxHash      string `json:"tx_hash,omitempty"`
	Prev        string `json:"prev"`
	Hash        string `json:"hash"`
}

func (e Entry) computeHash() (string, error) {
	prev, err := hex.DecodeString(e.Prev)
	if err != nil || len(prev) != 32 {
		return "", fmt.Errorf("bad prev %q", e.Prev)
	}
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Log is the open, appendable log.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  uint64
	head string
	err  error // sticky: after a failed write the file may hold a torn line; fail closed until restart
	now  func() time.Time
}

// Opened describes what Open found.
type Opened struct {
	Entries     []Entry // every valid entry, in order (for rebuilding Policy Windows)
	TrimmedTorn bool    // a final line without newline (a crash mid-append, never fsynced) was removed
}

// Open creates the log if absent, verifies the whole chain, trims a torn final line, and
// returns the log positioned to append. A corrupt chain is an error: the gateway must not start.
func Open(path string) (*Log, *Opened, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, err
	}
	res, err := scan(path, true)
	if err != nil {
		return nil, nil, err
	}
	opened := &Opened{Entries: res.entries, TrimmedTorn: res.tornAt >= 0}
	if res.tornAt >= 0 {
		if err := os.Truncate(path, res.tornAt); err != nil {
			return nil, nil, fmt.Errorf("audit: trimming torn tail: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, nil, err
	}
	if res.created {
		if err := syncDir(filepath.Dir(path)); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
	}
	l := &Log{f: f, path: path, seq: res.seq, head: res.head, now: time.Now}
	if l.head == "" {
		l.head = ZeroHash
	}
	return l, opened, nil
}

// SetClock overrides the timestamp source (tests).
func (l *Log) SetClock(now func() time.Time) { l.now = now }

// Append assigns seq, ts, prev and hash, writes one line and fsyncs before returning. It returns
// the appended entry. Any error makes the log unusable until restart.
func (l *Log) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return Entry{}, fmt.Errorf("audit log unusable: %w", l.err)
	}
	e.Seq = l.seq + 1
	if e.TS == "" {
		e.TS = l.now().UTC().Format(time.RFC3339Nano)
	}
	e.Prev = l.head
	h, err := e.computeHash()
	if err != nil {
		return Entry{}, err
	}
	e.Hash = h
	b, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	b = append(b, '\n')
	if _, err := l.f.Write(b); err != nil {
		l.err = err
		return Entry{}, fmt.Errorf("audit write: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		l.err = err
		return Entry{}, fmt.Errorf("audit fsync: %w", err)
	}
	l.seq, l.head = e.Seq, e.Hash
	return e, nil
}

// Head returns the sequence number and hash of the last entry (0, ZeroHash when empty).
func (l *Log) Head() (uint64, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq, l.head
}

// Healthy reports whether appends can still succeed.
func (l *Log) Healthy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err == nil
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// VerifyOptions optionally pin the expected head, which is what detects tail truncation.
type VerifyOptions struct {
	ExpectSeq  *uint64
	ExpectHash string
}

// Verify checks the chain in path: consecutive sequence numbers, correct prev links and hashes, a
// complete final line, and (when given) that the head equals the expected (seq, hash). It
// returns the head it found.
func Verify(path string, opts VerifyOptions) (seq uint64, head string, err error) {
	res, err := scan(path, false)
	if err != nil {
		return 0, "", err
	}
	if res.tornAt >= 0 {
		return res.seq, res.head, fmt.Errorf("audit: torn final line at byte %d (incomplete write or truncation)", res.tornAt)
	}
	if opts.ExpectSeq != nil && *opts.ExpectSeq != res.seq {
		return res.seq, res.head, fmt.Errorf("audit: head seq is %d, expected %d (truncated or rewritten)", res.seq, *opts.ExpectSeq)
	}
	if opts.ExpectHash != "" && opts.ExpectHash != res.head {
		return res.seq, res.head, fmt.Errorf("audit: head hash %s does not match expected %s", res.head, opts.ExpectHash)
	}
	return res.seq, res.head, nil
}

type scanned struct {
	entries []Entry
	seq     uint64
	head    string
	tornAt  int64 // byte offset where a torn final line starts, -1 if none
	created bool
}

func scan(path string, allowMissing bool) (*scanned, error) {
	res := &scanned{tornAt: -1}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		res.created = true
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	prev := ZeroHash
	var off int64
	for line := 1; ; line++ {
		b, rerr := r.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, rerr
		}
		if len(b) == 0 {
			break
		}
		if b[len(b)-1] != '\n' { // incomplete final line
			res.tornAt = off
			break
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("audit: line %d: %w", line, err)
		}
		if e.Seq != res.seq+1 {
			return nil, fmt.Errorf("audit: line %d: sequence %d, expected %d (entry removed or reordered)", line, e.Seq, res.seq+1)
		}
		if e.Prev != prev {
			return nil, fmt.Errorf("audit: line %d: prev hash does not match the previous entry (chain broken)", line)
		}
		want, err := e.computeHash()
		if err != nil || want != e.Hash {
			return nil, fmt.Errorf("audit: line %d: hash mismatch (entry modified)", line)
		}
		res.entries = append(res.entries, e)
		res.seq, res.head, prev = e.Seq, e.Hash, e.Hash
		off += int64(len(b))
		if rerr == io.EOF {
			break
		}
	}
	return res, nil
}
