package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seed() []byte {
	s := make([]byte, 64)
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

type src struct {
	row Row
	err error
}

func (s src) ActiveSecret(context.Context) (Row, error) { return s.row, s.err }

type gw struct {
	seed []byte
	err  error
	n    int
	got  Row
}

func (g *gw) UnsealInto(_ context.Context, r Row, dst []byte) (int, error) {
	g.n++
	g.got = r
	if g.err != nil {
		return 0, g.err
	}
	return copy(dst, g.seed), nil
}

func TestProvider_WithGivesTheSeedAndPanicStillPropagates(t *testing.T) {
	g := &gw{seed: seed()}
	p := &Provider{Source: src{row: Row{Role: "user_derivation", Version: 3}}, Unsealer: g}
	if err := p.With(context.Background(), func(s []byte) error {
		if !bytes.Equal(s, seed()) {
			t.Error("wrong seed")
		}
		return nil
	}); err != nil || p.Unseals() != 1 || g.got.Version != 3 {
		t.Fatalf("%v %d %+v", err, p.Unseals(), g.got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("panic swallowed")
		}
	}()
	_ = p.With(context.Background(), func([]byte) error { panic("boom") })
}

// The gateway being down, refusing, tampered or answering the wrong size means no Seed: the callback never runs.
func TestProvider_FailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		s src
		g *gw
	}{
		"no active row":  {src{err: errors.New("not found")}, &gw{seed: seed()}},
		"gateway down":   {src{}, &gw{err: errors.New("hsm-gateway unreachable")}},
		"authentication": {src{}, &gw{err: ErrAuthFailed}},
		"short answer":   {src{}, &gw{seed: seed()[:10]}},
	} {
		p := &Provider{Source: tc.s, Unsealer: tc.g}
		err := p.With(context.Background(), func([]byte) error { t.Errorf("%s: callback ran", name); return nil })
		if !errors.Is(err, ErrSeedUnavailable) || p.Failures() != 1 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGateway_UnsealIntoReadsTheRawBodyAndRefusesEverythingElse(t *testing.T) {
	mode := "ok"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ok":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(seed())
		case "json200":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(seed())
		case "long":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(append(seed(), 1))
		case "auth":
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"error":"authentication_failed"}`))
		case "rate":
			w.WriteHeader(429)
		case "403":
			w.WriteHeader(403)
		}
	}))
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	g := NewGateway(ts.URL, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13})
	dst := make([]byte, 64)
	if n, err := g.UnsealInto(context.Background(), Row{}, dst); err != nil || n != 64 || !bytes.Equal(dst, seed()) {
		t.Fatalf("ok: %d %v", n, err)
	}
	for mode, want := range map[string]string{"json200": "content type", "long": "more bytes", "auth": "authenticate", "rate": "rate limit", "403": "403"} {
		_ = mode
		_ = want
	}
	for _, c := range []struct{ m, want string }{{"json200", "content type"}, {"long", "more bytes"}, {"auth", "authenticate"}, {"rate", "rate limit"}, {"403", "403"}} {
		mode = c.m
		dst = make([]byte, 64)
		if _, err := g.UnsealInto(context.Background(), Row{}, dst); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.m, err)
		}
		if c.m == "long" && !bytes.Equal(dst, make([]byte, 64)) {
			t.Error("a too-long answer left bytes in the page")
		}
	}
	mode = "auth"
	if _, err := g.UnsealInto(context.Background(), Row{}, dst); !errors.Is(err, ErrAuthFailed) {
		t.Errorf("auth: %v", err)
	}
	ts.Close()
	if _, err := g.UnsealInto(context.Background(), Row{}, dst); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("gateway down: %v", err)
	}
}
