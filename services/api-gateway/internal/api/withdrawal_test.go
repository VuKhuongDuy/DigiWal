package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeWithdrawals stands in for withdrawal-service: it records each request (method, URI and
// decoded body) and answers like the real one would for the happy path.
type fakeWithdrawals struct {
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	status   int // override for every answer when set
}

func (f *fakeWithdrawals) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		f.bodies = append(f.bodies, b)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case f.status != 0:
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"upstream says no"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/withdrawals":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"idempotency_key":"k1","status":"reserved","tx_hash":null}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/withdrawals/"):
			_, _ = w.Write([]byte(`{"idempotency_key":"k1","status":"confirmed","tx_hash":"0xabc"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/whitelist":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"e1","status":"pending"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/whitelist":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})
}

const wdBody = `{"idempotency_key":"k1","asset_id":"11111111-1111-1111-1111-111111111111","amount":"1000","to_address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"}`

func TestWithdrawalRoutesCarryOnlyTheAuthenticatedUser(t *testing.T) {
	e := newEnv(t)
	aliceID, aliceKey := e.newUser()
	bobID, _ := e.newUser()
	auth := "Bearer " + aliceKey
	hdr := []string{"X-User-Id", bobID}

	code, body := e.do("POST", "/v1/withdrawals?user_id="+bobID, auth, wdBody, hdr...)
	if code != 202 || !strings.Contains(body, `"status":"reserved"`) {
		t.Fatalf("POST withdrawals: %d %s", code, body)
	}
	b := e.wd.bodies[0]
	if b["user_id"] != aliceID || b["idempotency_key"] != "k1" || b["amount"] != "1000" || len(b) != 5 {
		t.Fatalf("forwarded body = %v", b)
	}
	code, body = e.do("GET", "/v1/withdrawals/k1?user_id="+bobID, auth, "", hdr...)
	if code != 200 || !strings.Contains(body, `"status":"confirmed"`) {
		t.Fatalf("GET withdrawal: %d %s", code, body)
	}
	code, _ = e.do("POST", "/v1/whitelist", auth, `{"address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"}`, hdr...)
	if code != 201 || e.wd.bodies[2]["user_id"] != aliceID || len(e.wd.bodies[2]) != 2 {
		t.Fatalf("POST whitelist: %d, forwarded %v", code, e.wd.bodies[2])
	}
	if code, body = e.do("GET", "/v1/whitelist?user_id="+bobID, auth, "", hdr...); code != 200 || body != "[]" {
		t.Fatalf("GET whitelist: %d %q", code, body)
	}
	if code, body = e.do("DELETE", "/v1/whitelist/e1?user_id="+bobID, auth, "", hdr...); code != 204 || body != "" {
		t.Fatalf("DELETE whitelist: %d %q", code, body)
	}
	want := []string{
		"POST /v1/withdrawals",
		"GET /v1/withdrawals/k1?user_id=" + aliceID,
		"POST /v1/whitelist",
		"GET /v1/whitelist?user_id=" + aliceID,
		"DELETE /v1/whitelist/e1?user_id=" + aliceID,
	}
	for i, w := range want {
		if e.wd.requests[i] != w {
			t.Errorf("request %d = %q, want %q", i, e.wd.requests[i], w)
		}
		if strings.Contains(e.wd.requests[i], bobID) {
			t.Errorf("request %d names bob: %q", i, e.wd.requests[i])
		}
	}
}

// A client may not supply user_id (or anything unexpected) in a body: 400, nothing forwarded.
func TestWithdrawalBodiesAreStrict(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	for _, tc := range [][2]string{
		{"/v1/withdrawals", strings.Replace(wdBody, `{`, `{"user_id":"bob",`, 1)},
		{"/v1/withdrawals", `{`},
		{"/v1/whitelist", `{"address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8","user_id":"bob"}`},
	} {
		if code, _ := e.do("POST", tc[0], "Bearer "+key, tc[1]); code != 400 {
			t.Errorf("POST %s %s = %d, want 400", tc[0], tc[1], code)
		}
	}
	if len(e.wd.requests) != 0 {
		t.Fatalf("forwarded: %v", e.wd.requests)
	}
}

// withdrawal-service's own answers (422 rejected, 404, 503) reach the client unchanged; a dead
// upstream is a 502.
func TestWithdrawalAnswersArePassedThrough(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	for _, st := range []int{422, 404, 409, 503} {
		e.wd.status = st
		if code, body := e.do("POST", "/v1/withdrawals", "Bearer "+key, wdBody); code != st || !strings.Contains(body, "upstream says no") {
			t.Errorf("upstream %d -> %d %s", st, code, body)
		}
	}
}
