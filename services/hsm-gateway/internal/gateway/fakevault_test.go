package gateway

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"digiwal/services/hsm-gateway/internal/hsm"
)

// fakeVault is a TEST-ONLY stand-in for the token's KEK/DEK capability (a _test file: the service binary has no software crypto path).
// It models what the token guarantees that the gateway logic relies on: the AAD is bound by GCM, a wrapped DEK only opens under its own
// KEK, a retired KEK still unwraps but cannot wrap, versions are consecutive. The real AES-KEY-WRAP and CKM_AES_GCM are exercised by
// the SoftHSM tests (make test-softhsm).
type fakeVault struct {
	mu   sync.Mutex
	keks map[int]*fakeKEK

	unseals, seals, reseals atomic.Int64
	onUnseal                func() // runs when the token is asked to unseal (ordering assertions)
}

type fakeKEK struct {
	key  [32]byte
	wrap bool
}

func newFakeVault() *fakeVault {
	f := &fakeVault{keks: map[int]*fakeKEK{}}
	_, _ = f.GenerateKEK(context.Background(), 1)
	return f
}

func (f *fakeVault) HasKEK() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.keks) > 0 }

func (f *fakeVault) KEKs(context.Context) ([]hsm.KEKInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hsm.KEKInfo
	for v, k := range f.keks {
		out = append(out, hsm.KEKInfo{Version: v, Wrap: k.wrap})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (f *fakeVault) GenerateKEK(_ context.Context, v int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keks[v]; ok {
		return false, nil
	}
	if v != len(f.keks)+1 {
		return false, fmt.Errorf("KEK version %d refused: versions are consecutive", v)
	}
	k := &fakeKEK{wrap: true}
	_, _ = rand.Read(k.key[:])
	f.keks[v] = k
	return true, nil
}

func (f *fakeVault) RetireKEK(_ context.Context, v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keks[v]
	if !ok {
		return hsm.ErrNoKEK
	}
	if v == len(f.keks) {
		return fmt.Errorf("the newest KEK cannot be retired")
	}
	k.wrap = false
	return nil
}

func (f *fakeVault) DestroyKEK(_ context.Context, v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keks[v]
	if !ok {
		return hsm.ErrNoKEK
	}
	if k.wrap {
		return fmt.Errorf("not retired")
	}
	delete(f.keks, v)
	return nil
}

func gcmOf(key []byte) cipher.AEAD {
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	return g
}

func (f *fakeVault) kek(v int, wantWrap bool) (*fakeKEK, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keks[v]
	if !ok {
		return nil, hsm.ErrNoKEK
	}
	if wantWrap && !k.wrap {
		return nil, fmt.Errorf("KEK retired")
	}
	return k, nil
}

func (f *fakeVault) wrapEncrypt(kv int, k *fakeKEK, dek, pt []byte, aadOf hsm.AADFunc) (*hsm.Sealed, error) {
	wn := make([]byte, 12)
	_, _ = rand.Read(wn)
	wrapped := append(wn, gcmOf(k.key[:]).Seal(nil, wn, dek, nil)...)
	aad := aadOf(wrapped)
	nonce, _ := newTestNonce()
	return &hsm.Sealed{KEKVersion: kv, WrappedDEK: wrapped, Nonce: nonce, Ciphertext: gcmOf(dek).Seal(nil, nonce, pt, aad), AAD: aad}, nil
}

func newTestNonce() ([]byte, error) {
	n := make([]byte, hsm.NonceSize)
	_, err := rand.Read(n)
	return n, err
}

func (f *fakeVault) Seal(_ context.Context, kv int, pt []byte, aadOf hsm.AADFunc) (*hsm.Sealed, error) {
	f.seals.Add(1)
	k, err := f.kek(kv, true)
	if err != nil {
		return nil, err
	}
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	return f.wrapEncrypt(kv, k, dek, pt, aadOf)
}

func (f *fakeVault) open(o hsm.Opened, expect []byte) (dek, pt []byte, err error) {
	if !bytes.Equal(o.AAD, expect) {
		return nil, nil, hsm.ErrAuthFailed
	}
	if err := hsm.CheckNonce(o.Nonce); err != nil {
		return nil, nil, err
	}
	k, err := f.kek(o.KEKVersion, false)
	if err != nil {
		return nil, nil, err
	}
	if len(o.WrappedDEK) < 12 {
		return nil, nil, hsm.ErrAuthFailed
	}
	dek, err = gcmOf(k.key[:]).Open(nil, o.WrappedDEK[:12], o.WrappedDEK[12:], nil)
	if err != nil {
		return nil, nil, hsm.ErrAuthFailed
	}
	pt, err = gcmOf(dek).Open(nil, o.Nonce, o.Ciphertext, o.AAD)
	if err != nil {
		return nil, nil, hsm.ErrAuthFailed
	}
	return dek, pt, nil
}

func (f *fakeVault) Unseal(_ context.Context, o hsm.Opened, expect []byte) ([]byte, error) {
	f.unseals.Add(1)
	if f.onUnseal != nil {
		f.onUnseal()
	}
	_, pt, err := f.open(o, expect)
	return pt, err
}

func (f *fakeVault) Reseal(_ context.Context, o hsm.Opened, expect []byte, to int, newDEK bool, aadOf hsm.AADFunc) (*hsm.Sealed, error) {
	f.reseals.Add(1)
	dek, pt, err := f.open(o, expect)
	if err != nil {
		return nil, err
	}
	k, err := f.kek(to, true)
	if err != nil {
		return nil, err
	}
	if newDEK {
		dek = make([]byte, 32)
		_, _ = rand.Read(dek)
	}
	return f.wrapEncrypt(to, k, dek, pt, aadOf)
}
