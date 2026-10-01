package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/store"
)

type fakeWhitelist struct {
	entries map[[2]string]store.WhitelistEntry // (user, address)
	now     time.Time
	calls   int
}

func newFakeWL() *fakeWhitelist {
	return &fakeWhitelist{entries: map[[2]string]store.WhitelistEntry{}, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeWhitelist) AddWhitelistEntry(_ context.Context, userID, address string) (store.WhitelistEntry, bool, error) {
	f.calls++
	k := [2]string{userID, address}
	if e, ok := f.entries[k]; ok {
		return e, false, nil
	}
	e := store.WhitelistEntry{ID: "11111111-2222-3333-4444-" + strings.Repeat("0", 11) + string(rune('0'+len(f.entries))),
		UserID: userID, Address: address, AddedAt: f.now}
	f.entries[k] = e
	return e, true, nil
}

func (f *fakeWhitelist) RemoveWhitelistEntry(_ context.Context, userID, id string) error {
	f.calls++
	for k, e := range f.entries {
		if e.ID == id && e.UserID == userID {
			delete(f.entries, k)
		}
	}
	return nil
}

func (f *fakeWhitelist) ListWhitelist(_ context.Context, userID string) ([]store.WhitelistEntry, error) {
	f.calls++
	var out []store.WhitelistEntry
	for _, e := range f.entries {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeWhitelist) Now(context.Context) (time.Time, error) { return f.now, nil }

type fakeDest struct {
	reason string
	err    error
}

func (d fakeDest) Refuse(context.Context, common.Address) (string, error) { return d.reason, d.err }

func doWL(t *testing.T, wl *fakeWhitelist, cfg Config, cn, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPeer(httptest.NewRequest(method, path, strings.NewReader(body)), cn)
	rec := httptest.NewRecorder()
	NewServer(newFake(), wl, cfg).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

const checksummed = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

func TestWhitelistOnlyTheGatewayMayCall(t *testing.T) {
	for _, cn := range []string{"sweeper", "withdrawal-service", ""} {
		wl := newFakeWL()
		if rec := doWL(t, wl, Config{}, cn, "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+checksummed+`"}`); rec.Code != 403 || wl.calls != 0 {
			t.Errorf("POST as CN=%q: %d", cn, rec.Code)
		}
		if rec := doWL(t, wl, Config{}, cn, "GET", "/v1/whitelist?user_id=alice", ""); rec.Code != 403 || wl.calls != 0 {
			t.Errorf("GET as CN=%q: %d", cn, rec.Code)
		}
		if rec := doWL(t, wl, Config{}, cn, "DELETE", "/v1/whitelist/x?user_id=alice", ""); rec.Code != 403 || wl.calls != 0 {
			t.Errorf("DELETE as CN=%q: %d", cn, rec.Code)
		}
	}
}

func TestWhitelistAdd(t *testing.T) {
	wl := newFakeWL()
	cfg := Config{WhitelistCooldown: 24 * time.Hour}
	// Lower-case input is stored in EIP-55 form, so both spellings are one entry.
	rec := doWL(t, wl, cfg, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+strings.ToLower(checksummed)+`"}`)
	if rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var got whitelistResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Address != checksummed || got.Status != "pending" || got.UserID != "alice" || got.ID == "" {
		t.Fatalf("add body: %+v (%v)", got, err)
	}
	rec = doWL(t, wl, cfg, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+checksummed+`"}`)
	var again whitelistResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != 200 || again.ID != got.ID || !again.AddedAt.Equal(got.AddedAt) || len(wl.entries) != 1 {
		t.Fatalf("re-add: %d %+v (entries %d)", rec.Code, again, len(wl.entries))
	}
	// Once the cooldown has passed, the same entry reads back as active.
	wl.now = wl.now.Add(24 * time.Hour)
	rec = doWL(t, wl, cfg, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+checksummed+`"}`)
	if !strings.Contains(rec.Body.String(), `"status":"active"`) {
		t.Fatalf("after cooldown: %s", rec.Body)
	}
}

func TestWhitelistAddRefusesBadInput(t *testing.T) {
	cases := map[string]string{
		"not json":      `{`,
		"unknown field": `{"user_id":"alice","address":"` + checksummed + `","status":"active"}`,
		"no user":       `{"address":"` + checksummed + `"}`,
		"short address": `{"user_id":"alice","address":"0x1234"}`,
		"no 0x":         `{"user_id":"alice","address":"70997970C51812dc3A010C7d01b50e0d17dc79C8"}`,
		"zero address":  `{"user_id":"alice","address":"0x0000000000000000000000000000000000000000"}`,
	}
	for name, b := range cases {
		wl := newFakeWL()
		if rec := doWL(t, wl, Config{}, "api-gateway", "POST", "/v1/whitelist", b); rec.Code != 400 || len(wl.entries) != 0 {
			t.Errorf("%s: %d, want 400 and nothing stored", name, rec.Code)
		}
	}
}

// The invariant #18 hook (wired in task 3.10): a refused destination is 422 with the reason, a
// failed check is 503, and neither creates an entry.
func TestWhitelistAddAsksTheDestinationCheck(t *testing.T) {
	body := `{"user_id":"alice","address":"` + checksummed + `"}`
	wl := newFakeWL()
	rec := doWL(t, wl, Config{Destinations: fakeDest{reason: "destination_is_deposit_address"}}, "api-gateway", "POST", "/v1/whitelist", body)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "destination_is_deposit_address") || len(wl.entries) != 0 {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	rec = doWL(t, wl, Config{Destinations: fakeDest{err: errors.New("wallet-service down")}}, "api-gateway", "POST", "/v1/whitelist", body)
	if rec.Code != 503 || len(wl.entries) != 0 {
		t.Fatalf("check failed: %d, want 503 (fail closed)", rec.Code)
	}
	if rec := doWL(t, wl, Config{Destinations: fakeDest{}}, "api-gateway", "POST", "/v1/whitelist", body); rec.Code != 201 {
		t.Fatalf("allowed: %d", rec.Code)
	}
}

func TestWhitelistRemove(t *testing.T) {
	wl := newFakeWL()
	doWL(t, wl, Config{}, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+checksummed+`"}`)
	var id string
	for _, e := range wl.entries {
		id = e.ID
	}
	if rec := doWL(t, wl, Config{}, "api-gateway", "DELETE", "/v1/whitelist/"+id+"?user_id=bob", ""); rec.Code != 204 || len(wl.entries) != 1 {
		t.Fatalf("bob deleting alice's entry: %d (entries %d), want a 204 no-op", rec.Code, len(wl.entries))
	}
	if rec := doWL(t, wl, Config{}, "api-gateway", "DELETE", "/v1/whitelist/"+id+"?user_id=alice", ""); rec.Code != 204 || len(wl.entries) != 0 {
		t.Fatalf("alice deleting her entry: %d (entries %d)", rec.Code, len(wl.entries))
	}
	if rec := doWL(t, wl, Config{}, "api-gateway", "DELETE", "/v1/whitelist/"+id+"?user_id=alice", ""); rec.Code != 204 {
		t.Fatalf("second delete: %d, want 204", rec.Code)
	}
	calls := wl.calls
	if rec := doWL(t, wl, Config{}, "api-gateway", "DELETE", "/v1/whitelist/not-a-uuid?user_id=alice", ""); rec.Code != 204 || wl.calls != calls {
		t.Fatalf("malformed id: %d (store calls %d -> %d), want 204 without a store call", rec.Code, calls, wl.calls)
	}
	if rec := doWL(t, wl, Config{}, "api-gateway", "DELETE", "/v1/whitelist/"+id, ""); rec.Code != 400 {
		t.Fatalf("no user_id: %d, want 400", rec.Code)
	}
}

func TestWhitelistList(t *testing.T) {
	wl := newFakeWL()
	cfg := Config{WhitelistCooldown: 24 * time.Hour}
	if rec := doWL(t, wl, cfg, "api-gateway", "GET", "/v1/whitelist?user_id=alice", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %q, want 200 []", rec.Code, rec.Body)
	}
	doWL(t, wl, cfg, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"alice","address":"`+checksummed+`"}`)
	doWL(t, wl, cfg, "api-gateway", "POST", "/v1/whitelist", `{"user_id":"bob","address":"`+checksummed+`"}`)
	rec := doWL(t, wl, cfg, "api-gateway", "GET", "/v1/whitelist?user_id=alice", "")
	var items []whitelistItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0].Address != checksummed || items[0].Status != "pending" {
		t.Fatalf("alice's list: %s (%v)", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), "user_id") {
		t.Fatalf("list repeats user_id: %s", rec.Body)
	}
	if rec := doWL(t, wl, cfg, "api-gateway", "GET", "/v1/whitelist", ""); rec.Code != 400 {
		t.Fatalf("no user_id: %d", rec.Code)
	}
}
