package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
	"digiwal/services/wallet-service/internal/xpub"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

func attestWith(k *ecdsa.PrivateKey, xp string) []byte {
	d := xpub.Digest(xpub.ChainEVM, xpub.PathEVM, xp)
	r, s, _ := ecdsa.Sign(rand.Reader, k, d[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func deposit(t *testing.T, srv *Server, method, user string) (int, map[string]string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(method, "/v1/users/"+user+"/deposit-address", nil))
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

// The HTTP behaviour of M3a: with xpub issuance on and NO seed loaded (signer == nil) a new user gets the address the seed
// would have derived; after the row is tampered with new users get 503 issuance_unavailable while stored addresses still serve.
func TestDepositAddress_FromXPubWithoutASeedAndFailClosedOnTamper(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.New(t)
	store, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mn, _ := hdwallet.NewMnemonic()
	w, _ := hdwallet.FromMnemonic(mn)
	xp, _ := hdwallet.AccountXPub(mn)
	hsm, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := store.InsertXPub(ctx, storage.XPubRow{Chain: "evm", Path: xpub.PathEVM, Depth: 3, XPub: xp, Attestation: attestWith(hsm, xp), NextIndex: 0}); err != nil {
		t.Fatal(err)
	}
	iss, _ := xpub.NewIssuer(&hsm.PublicKey)
	srv := NewServer(store, nil, nil, nil).WithXPubIssuance(iss) // no Signer: no seed anywhere

	if code, _, _ := deposit(t, srv, "GET", "alice"); code != 404 {
		t.Fatalf("GET before creation: %d", code)
	}
	code, m, body := deposit(t, srv, "POST", "alice")
	want, _ := w.AddressAt(0)
	if code != 201 || m["address"] != want {
		t.Fatalf("POST: %d %s, want 201 %s", code, body, want)
	}
	if strings.Contains(body, "xpub") || strings.Contains(body, xp) {
		t.Fatalf("the response leaks the xpub: %s", body)
	}
	if code, m, _ := deposit(t, srv, "GET", "alice"); code != 200 || m["address"] != want {
		t.Fatalf("GET: %d %v", code, m)
	}
	if code, m, _ := deposit(t, srv, "POST", "bob"); code != 201 || m["address"] == want {
		t.Fatalf("second user: %d %v", code, m)
	}

	// the attacker swaps the xpub in the database
	other, _ := hdwallet.NewMnemonic()
	ox, _ := hdwallet.AccountXPub(other)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE wallet_xpubs SET xpub = $1`, ox); err != nil {
		t.Fatal(err)
	}
	code, m, body = deposit(t, srv, "POST", "mallory-victim")
	if code != 503 || m["error"] != "issuance_unavailable" || strings.Contains(body, "0x") {
		t.Fatalf("swapped xpub: %d %s, want 503 issuance_unavailable", code, body)
	}
	if !iss.Latched() {
		t.Fatal("issuance did not latch")
	}
	// latched: 503 for creation without touching the DB again; existing users unaffected; unknown GET still 404
	if code, _, _ := deposit(t, srv, "POST", "another"); code != 503 {
		t.Fatalf("latched POST: %d", code)
	}
	if code, m, _ := deposit(t, srv, "GET", "alice"); code != 200 || m["address"] != want {
		t.Fatalf("existing user while latched: %d %v", code, m)
	}
	if code, _, _ := deposit(t, srv, "GET", "never-seen"); code != 404 {
		t.Fatalf("GET unknown while latched: %d", code)
	}
}

func TestDepositAddress_NoXPubRowIs503NotAPanic(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hsm, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	iss, _ := xpub.NewIssuer(&hsm.PublicKey)
	srv := NewServer(store, nil, nil, nil).WithXPubIssuance(iss)
	if code, m, _ := deposit(t, srv, "POST", "u"); code != http.StatusServiceUnavailable || m["error"] != "issuance_unavailable" {
		t.Fatalf("%d %v", code, m)
	}
	if iss.Latched() {
		t.Fatal("a missing row is not tampering: it must not latch")
	}
}

// Without xpub issuance the seed path is exactly what it was.
func TestDepositAddress_SeedPathUnchangedWhenXPubIssuanceIsOff(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InitSeed(ctx, storage.RoleUserDerivation, []byte("s")); err != nil {
		t.Fatal(err)
	}
	mn, _ := hdwallet.NewMnemonic()
	w, _ := hdwallet.FromMnemonic(mn)
	srv := NewServer(store, nil, w, nil)
	want, _ := w.AddressAt(0)
	if code, m, _ := deposit(t, srv, "POST", "alice"); code != 201 || m["address"] != want {
		t.Fatalf("%d %v", code, m)
	}
	// and with neither a seed nor xpub issuance: still the old 503
	if code, _, _ := deposit(t, NewServer(store, nil, nil, nil), "POST", "bob"); code != 503 {
		t.Fatalf("no signer: %d", code)
	}
}
