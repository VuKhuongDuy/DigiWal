package intent

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const body = `{"chain_id":31337,"nonce":7,"to":"0x5555555555555555555555555555555555555555","value":"1000","data":"0x","gas":21000,` +
	`"max_fee_per_gas":"2000000000","max_priority_fee_per_gas":"1000000000","operation":"payout","idempotency_key":"k1","caller_cn":"wallet-service"}`

func TestDecode_RejectsCallerSuppliedDigestAndUnknownFields(t *testing.T) {
	if _, err := Decode(strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`"digest":"0x00"`, `"hash":"0x00"`, `"signature":"0x00"`, `"raw_tx":"0x00"`, `"type":1`, `"access_list":[]`} {
		withExtra := strings.Replace(body, `"nonce":7,`, `"nonce":7,`+extra+`,`, 1)
		if _, err := Decode(strings.NewReader(withExtra)); err == nil {
			t.Fatalf("a body with %s was accepted", extra)
		}
	}
	if _, err := Decode(strings.NewReader(body + body)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestParse_Validation(t *testing.T) {
	good := func() Wire {
		in, _ := Decode(strings.NewReader(body))
		return Wire{ChainID: in.ChainID, Nonce: in.Nonce, To: in.To.Hex(), Value: "1", Data: "0x", Gas: 21000, MaxFeePerGas: "1", MaxPriorityFeePerGas: "1", Operation: OpPayout, IdempotencyKey: "k"}
	}
	for name, mut := range map[string]func(*Wire){
		"no chain":       func(w *Wire) { w.ChainID = 0 },
		"short to":       func(w *Wire) { w.To = "0x1234" },
		"empty to":       func(w *Wire) { w.To = "" }, // no contract creation
		"negative value": func(w *Wire) { w.Value = "-1" },
		"hex value":      func(w *Wire) { w.Value = "0x10" },
		"huge value":     func(w *Wire) { w.Value = "1" + strings.Repeat("0", 80) },
		"odd data":       func(w *Wire) { w.Data = "0xabc" },
		"data no prefix": func(w *Wire) { w.Data = "abcd" },
		"zero gas":       func(w *Wire) { w.Gas = 0 },
		"no idempotency": func(w *Wire) { w.IdempotencyKey = "" },
		"empty fee":      func(w *Wire) { w.MaxFeePerGas = "" },
	} {
		w := good()
		mut(&w)
		if _, err := Parse(w); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(good()); err != nil {
		t.Fatal(err)
	}
}

// The digest is rebuilt from the intent: it equals the go-ethereum signing hash of the same EIP-1559 tx.
func TestDigestAndSeal(t *testing.T) {
	in, err := Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(31337), Nonce: 7, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(2e9),
		Gas: 21000, To: &in.To, Value: big.NewInt(1000)})
	signer := types.LatestSignerForChainID(big.NewInt(31337))
	if in.Digest() != signer.Hash(want) {
		t.Fatal("digest differs from the go-ethereum signing hash of the same transaction")
	}
	if in.Tx().Type() != types.DynamicFeeTxType || len(in.Tx().AccessList()) != 0 || in.Tx().To() == nil {
		t.Fatal("not a type-2 tx with empty access list and a recipient")
	}
	key, _ := crypto.GenerateKey()
	d := in.Digest()
	sig, _ := crypto.Sign(d[:], key)
	tx, raw, err := in.Seal(sig)
	if err != nil {
		t.Fatal(err)
	}
	from, err := types.Sender(signer, tx)
	if err != nil || from != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("sender %v err %v", from, err)
	}
	var back types.Transaction
	if err := back.UnmarshalBinary(raw); err != nil || back.Hash() != tx.Hash() {
		t.Fatal("raw bytes do not round-trip")
	}
}

func TestSelfTestIntentCanNeverBeMined(t *testing.T) {
	in := SelfTestIntent(31337, common.HexToAddress("0x1111111111111111111111111111111111111111"))
	if in.Nonce != ^uint64(0) {
		t.Fatalf("self-test nonce is %d, want 2^64-1 (EIP-2681)", in.Nonce)
	}
}
