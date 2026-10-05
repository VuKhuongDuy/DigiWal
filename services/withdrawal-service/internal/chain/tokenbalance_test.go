package chain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// rpcStub answers eth_call with `result` and records the call data.
func rpcStub(t *testing.T, result string, gotData *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "eth_call" {
			var call struct{ Data, Input string }
			_ = json.Unmarshal(req.Params[0], &call)
			*gotData = call.Input + call.Data
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":"` + result + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTokenBalance(t *testing.T) {
	var data string
	url := rpcStub(t, "0x"+strings.Repeat("0", 62)+"2a", &data)
	e, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	holder := common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3")
	got, err := e.TokenBalance(context.Background(), common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"), holder)
	if err != nil || got.Int64() != 42 {
		t.Fatalf("got %v, %v", got, err)
	}
	// balanceOf(address): selector 70a08231 + the holder left-padded to 32 bytes
	if want := "0x70a08231" + strings.Repeat("0", 24) + strings.ToLower(holder.Hex()[2:]); !strings.EqualFold(data, want) {
		t.Fatalf("call data = %s, want %s", data, want)
	}
}

// A node answers "0x" for a call to an address with no contract: that is an error, not a balance of zero.
func TestTokenBalanceWithNoContractIsAnError(t *testing.T) {
	var data string
	e, err := Dial(context.Background(), rpcStub(t, "0x", &data))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.TokenBalance(context.Background(), common.HexToAddress("0x01"), common.HexToAddress("0x02")); err == nil {
		t.Fatalf("a missing contract read as %v", v)
	}
}
