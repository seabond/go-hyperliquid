package hyperliquid

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// EncodeAction must return the bytes actionHash actually consumes. If it ever
// diverged, a remote signer would be shown one action while a different one was
// hashed and sent — which is exactly the confusion exporting it exists to remove.
//
// Each case is run repeatedly. A single comparison would not catch the map case:
// Go randomises map iteration order, so two independent encodes agree by luck
// often enough that one round would pass on most runs.
func TestEncodeActionMatchesWhatActionHashConsumes(t *testing.T) {
	type Order struct {
		Type     string `msgpack:"type"`
		Grouping string `msgpack:"grouping"`
	}
	cases := []struct {
		name      string
		action    any
		vaultAddr string
		nonce     int64
	}{
		{"order", Order{Type: "order", Grouping: "na"}, "", 1700000000000},
		{"with vault", Order{Type: "cancel", Grouping: "na"}, "0x2222222222222222222222222222222222222222", 7},
		// A user-signed action reaches the SDK as a map, which is where the two
		// encoders used to disagree. Six keys give 720 orderings, so an encoder
		// that does not fix the order has essentially no chance of matching.
		{"map action", map[string]any{
			"type":             "usdSend",
			"destination":      "0x1111111111111111111111111111111111111111",
			"amount":           "10.5",
			"time":             uint64(1700000000000),
			"signatureChainId": "0x66eee",
			"hyperliquidChain": "Mainnet",
		}, "", 1700000000000},
		// Nesting must be ordered at every level, not just the top one.
		{"nested map action", map[string]any{
			"type": "multiSig",
			"action": map[string]any{
				"type":        "withdraw3",
				"destination": "0x2222222222222222222222222222222222222222",
				"amount":      "1000",
				"time":        uint64(42),
			},
			"signers":    []any{"0xaaaa", "0xbbbb"},
			"signatures": []any{map[string]any{"r": "0x1", "s": "0x2", "v": uint64(27)}},
		}, "0x3333333333333333333333333333333333333333", 9},
		// Strings on both sides of the str16-to-str8 boundary. Under 256 bytes the
		// header is rewritten; at or over it, it is left alone. A rewrite applied
		// to the wrong one would shift every following byte.
		{"strings across the str8 boundary", map[string]any{
			"type":  "order",
			"short": strings.Repeat("a", 255),
			"long":  strings.Repeat("b", 256),
			"huge":  strings.Repeat("c", 4096),
		}, "", 5},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := ActionHash(c.action, c.vaultAddr, c.nonce, nil)
			var first []byte
			for i := 0; i < 32; i++ {
				mp, err := EncodeAction(c.action)
				if err != nil {
					t.Fatal(err)
				}
				if len(mp) == 0 {
					t.Fatal("EncodeAction returned no bytes")
				}
				if first == nil {
					first = mp
				} else if !bytes.Equal(mp, first) {
					t.Fatalf("round %d: EncodeAction is not deterministic\n got %x\nwant %x", i, mp, first)
				}

				// Rebuild the hash from the exported bytes the way a remote signer
				// would, and compare with the SDK's own.
				got := hashFromEncoded(t, mp, c.vaultAddr, c.nonce)
				if !bytes.Equal(got, want) {
					t.Fatalf("round %d: hash from exported bytes\n got %x\nwant %x", i, got, want)
				}
				// The SDK's own hash must be stable too, for the same reason.
				if again := ActionHash(c.action, c.vaultAddr, c.nonce, nil); !bytes.Equal(again, want) {
					t.Fatalf("round %d: ActionHash is not deterministic\n got %x\nwant %x", i, again, want)
				}
			}
		})
	}
}

// Ordering map keys must not reorder struct fields. Hyperliquid hashes an action
// in the order Python's insertion-ordered dict produced, which for a Go struct
// is declaration order — sorting those would change every order's hash.
func TestStructFieldsKeepDeclarationOrder(t *testing.T) {
	type Order struct {
		Type     string `msgpack:"type"`
		Grouping string `msgpack:"grouping"`
		Asset    string `msgpack:"asset"`
	}
	mp, err := EncodeAction(Order{Type: "order", Grouping: "na", Asset: "ETH"})
	if err != nil {
		t.Fatal(err)
	}
	// fixmap(3), then the keys in declaration order. Sorted would put "asset"
	// first.
	want := []byte{0x83}
	for _, k := range []string{"type", "grouping", "asset"} {
		want = append(want, byte(0xa0|len(k)))
		want = append(want, k...)
		switch k {
		case "type":
			want = append(want, 0xa5, 'o', 'r', 'd', 'e', 'r')
		case "grouping":
			want = append(want, 0xa2, 'n', 'a')
		case "asset":
			want = append(want, 0xa3, 'E', 'T', 'H')
		}
	}
	if !bytes.Equal(mp, want) {
		t.Fatalf("struct field order\n got %x\nwant %x", mp, want)
	}
}

// A map the encoder cannot order has no single hash, so it must be refused
// rather than signed over bytes nobody can reproduce.
func TestUnorderableMapsAreRefused(t *testing.T) {
	type Action struct {
		Type   string         `msgpack:"type"`
		Limits map[string]int `msgpack:"limits"`
	}
	if _, err := EncodeAction(Action{Type: "order", Limits: map[string]int{"a": 1, "b": 2}}); err == nil {
		t.Fatal("a map[string]int action was encoded; its key order is not fixed, so its hash is not either")
	}
	// The orderable kinds must still work, at every nesting level.
	if _, err := EncodeAction(map[string]any{
		"type": "order",
		"tags": map[string]string{"a": "1"},
		"on":   map[string]bool{"b": true},
	}); err != nil {
		t.Fatalf("an orderable map was refused: %v", err)
	}
}

// A remote signer asked to authorize a transfer builds its request from
// UserSignedActionHashes. Those hashes must be exactly what the in-process path
// signs, so the test signs an action the normal way and checks the signature
// recovers over the exported hashes — comparing the two payload structs would
// only prove they share a constructor.
func TestUserSignedActionHashesMatchTheDefaultSigningPath(t *testing.T) {
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe512961708279e8f6f0e0f0f0f0f0f0")
	if err != nil {
		t.Fatal(err)
	}
	account := NewAccount(key)

	usdSendTypes := []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "destination", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "time", Type: "uint64"},
	}
	for _, mainnet := range []bool{true, false} {
		name := "testnet"
		if mainnet {
			name = "mainnet"
		}
		t.Run(name, func(t *testing.T) {
			action := map[string]any{
				"type":        "usdSend",
				"destination": "0x1111111111111111111111111111111111111111",
				"amount":      "10.5",
				"time":        big.NewInt(1700000000000),
			}

			ds, tdh, err := UserSignedActionHashes(action, usdSendTypes,
				"HyperliquidTransaction:UsdSend", mainnet)
			if err != nil {
				t.Fatal(err)
			}
			// Asking about an action must not change it. The signing path adds the
			// envelope on purpose; this must not, or a caller inspecting an action
			// would silently alter what it later sends.
			if _, added := action["hyperliquidChain"]; added {
				t.Fatal("UserSignedActionHashes modified the caller's action")
			}

			sig, err := SignUserSignedAction(context.Background(), account, action,
				usdSendTypes, "HyperliquidTransaction:UsdSend", mainnet)
			if err != nil {
				t.Fatal(err)
			}

			raw := append([]byte{0x19, 0x01}, ds...)
			raw = append(raw, tdh...)
			digest := crypto.Keccak256(raw)

			r, _ := new(big.Int).SetString(strings.TrimPrefix(sig.R, "0x"), 16)
			s, _ := new(big.Int).SetString(strings.TrimPrefix(sig.S, "0x"), 16)
			var rs [65]byte
			r.FillBytes(rs[0:32])
			s.FillBytes(rs[32:64])
			rs[64] = byte(sig.V - 27)

			pub, err := crypto.SigToPub(digest, rs[:])
			if err != nil {
				t.Fatalf("recover over the exported hashes: %v", err)
			}
			if got, want := crypto.PubkeyToAddress(*pub), account.Address(); got != want {
				t.Fatalf("the exported hashes are not what was signed: recovered %s, want %s", got, want)
			}
		})
	}
}

// hashFromEncoded mirrors what a remote signer does with EncodeAction's output.
func hashFromEncoded(t *testing.T, mp []byte, vaultAddress string, nonce int64) []byte {
	t.Helper()
	data := append([]byte(nil), mp...)
	var n [8]byte
	for i := 0; i < 8; i++ {
		n[7-i] = byte(uint64(nonce) >> (8 * uint(i)))
	}
	data = append(data, n[:]...)
	if vaultAddress == "" {
		data = append(data, 0x00)
	} else {
		data = append(data, 0x01)
		data = append(data, addressToBytes(vaultAddress)...)
	}
	return crypto.Keccak256(data)
}

// A remote signer builds its request from L1ActionHashes. Those hashes must be
// exactly what the default in-process path signs, or Hyperliquid rejects the
// signature — and it would surface as a rejected order, not as an obvious bug.
func TestL1ActionHashesMatchTheDefaultSigningPath(t *testing.T) {
	type Order struct {
		Type     string `msgpack:"type"`
		Grouping string `msgpack:"grouping"`
	}
	cases := []struct {
		name      string
		vaultAddr string
		nonce     int64
		mainnet   bool
	}{
		{"mainnet", "", 1700000000000, true},
		{"testnet", "", 42, false},
		{"with vault address", "0x2222222222222222222222222222222222222222", 7, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action := Order{Type: "order", Grouping: "na"}

			// What the default path would sign.
			want := l1Payload(constructPhantomAgent(
				actionHash(action, c.vaultAddr, c.nonce, nil), c.mainnet), c.mainnet)
			wantDS, err := want.HashStruct("EIP712Domain", want.Domain.Map())
			if err != nil {
				t.Fatal(err)
			}
			wantTDH, err := want.HashStruct(want.PrimaryType, want.Message)
			if err != nil {
				t.Fatal(err)
			}

			gotDS, gotTDH, err := L1ActionHashes(action, c.vaultAddr, c.nonce, nil, c.mainnet)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotDS, wantDS) {
				t.Fatalf("domain separator\n got %x\nwant %x", gotDS, wantDS)
			}
			if !bytes.Equal(gotTDH, wantTDH) {
				t.Fatalf("typed data hash\n got %x\nwant %x", gotTDH, wantTDH)
			}
		})
	}
}

// Encoding a map-typed action must be DETERMINISTIC across calls.
//
// This is the property that was missing. Go randomises map iteration, so
// encoding the same action twice produced different bytes — and therefore a
// different action hash each time. The signature was then over a pre-image
// nobody could reproduce: not the exchange, which re-derives from the JSON body
// it received (and Go's encoding/json sorts map keys, so the body is in sorted
// order), and not an auditor.
//
// Map-typed actions were thus intermittently rejected, in a way that looks like
// a flaky exchange rather than a bug. 200 iterations makes a lucky pass
// vanishingly unlikely for the 3-key case below.
func TestMapActionEncodingIsDeterministic(t *testing.T) {
	action := map[string]any{
		"type":        "usdSend",
		"destination": "0x1111111111111111111111111111111111111111",
		"amount":      "10.5",
	}

	want, err := EncodeAction(action)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := ActionHash(action, "", 7, nil)

	for i := 0; i < 200; i++ {
		got, err := EncodeAction(action)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: encoding changed\n got %x\nwant %x", i, got, want)
		}
		if h := ActionHash(action, "", 7, nil); !bytes.Equal(h, wantHash) {
			t.Fatalf("iteration %d: action hash changed\n got %x\nwant %x", i, h, wantHash)
		}
	}

	// And the order must be lexicographic, which is what the exchange re-derives
	// from: encoding/json sorts map keys, so the body it parses is sorted.
	idx := func(k string) int { return bytes.Index(want, []byte(k)) }
	if !(idx("amount") < idx("destination") && idx("destination") < idx("type")) {
		t.Fatalf("map keys are not in lexicographic order: %x", want)
	}
}
