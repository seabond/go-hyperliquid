package hyperliquid

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// EncodeAction must return the bytes actionHash actually consumes. If it ever
// diverged, a remote signer would be shown one action while a different one was
// hashed and sent — which is exactly the confusion exporting it exists to remove.
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
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mp, err := EncodeAction(c.action)
			if err != nil {
				t.Fatal(err)
			}
			if len(mp) == 0 {
				t.Fatal("EncodeAction returned no bytes")
			}

			// Rebuild the hash from the exported bytes the way a remote signer
			// would, and compare with the SDK's own.
			want := ActionHash(c.action, c.vaultAddr, c.nonce, nil)
			got := hashFromEncoded(t, mp, c.vaultAddr, c.nonce)
			if !bytes.Equal(got, want) {
				t.Fatalf("hash from exported bytes\n got %x\nwant %x", got, want)
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
