package hyperliquid

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestSendToEvmActionMatchesAcceptedSignature pins the action's typed data to
// a withdrawal Hyperliquid actually accepted.
//
// The request, signature and signer below are transcribed from a live testnet
// sendToEvmWithData that answered {"status":"ok"} and delivered USDC on
// Polygon. Recovering that signature through this package's own field list
// proves the list, its order and the domain reproduce the payload the exchange
// verified. A client that only checks itself would sign a reordered payload
// happily and only discover it as a rejected withdrawal in production.
func TestSendToEvmActionMatchesAcceptedSignature(t *testing.T) {
	const (
		signer = "0x6c7e4bc2e3edAdB0bA9f841B4CBc05e566EbD045"
		sigR   = "0xf0877ddc7e91d3bd35d1495e82ad26f78f156726340ad0e6c4024eaf48aa17f4"
		sigS   = "0x36c962fbc4c44e5f17096c0acfb65c5145ca41a753291f48b802e824fda0460f"
		sigV   = 27
		nonce  = int64(1786953858604)
	)

	action, err := sendToEvmAction(SendToEvmRequest{
		Token:                "USDC",
		Amount:               "5",
		SourceDex:            "spot",
		DestinationRecipient: "0x662971350e886a0A5631D3E9133d33F767f80611",
		DestinationDomain:    7, // Polygon's CCTP domain, not chain id 137
	}, nonce)
	if err != nil {
		t.Fatalf("build action: %v", err)
	}

	domainSeparator, typedDataHash, err := UserSignedActionHashes(
		action, sendToEvmWithDataTypes, SendToEvmWithDataPrimaryType, false /* testnet */)
	if err != nil {
		t.Fatalf("hash action: %v", err)
	}
	digest := crypto.Keccak256([]byte{0x19, 0x01}, domainSeparator, typedDataHash)

	sig := make([]byte, 0, 65)
	sig = append(sig, common.FromHex(sigR)...)
	sig = append(sig, common.FromHex(sigS)...)
	sig = append(sig, byte(sigV-27))

	pub, err := crypto.SigToPub(digest, sig)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != common.HexToAddress(signer) {
		t.Fatalf("payload recovered to %s, want %s: the typed data no longer matches what Hyperliquid accepted",
			got.Hex(), signer)
	}
}

func TestSendToEvmActionDefaults(t *testing.T) {
	action, err := sendToEvmAction(SendToEvmRequest{
		Token:                "USDC",
		Amount:               "1",
		DestinationRecipient: "0x662971350e886a0A5631D3E9133d33F767f80611",
		DestinationDomain:    3,
	}, 1)
	if err != nil {
		t.Fatalf("build action: %v", err)
	}
	if got := action["addressEncoding"]; got != AddressEncodingHex {
		t.Errorf("addressEncoding = %v, want %s", got, AddressEncodingHex)
	}
	// Empty data is what asks the linked contract for its default behaviour,
	// so it has to reach the wire as "0x" rather than as an empty string.
	if got := action["data"]; got != "0x" {
		t.Errorf("data = %v, want 0x", got)
	}
	if got := action["sourceDex"]; got != "" {
		t.Errorf("sourceDex = %v, want the default perp dex", got)
	}
}

func TestSendToEvmActionRejectsIncomplete(t *testing.T) {
	for name, req := range map[string]SendToEvmRequest{
		"no token":     {Amount: "1", DestinationRecipient: "0x01"},
		"no amount":    {Token: "USDC", DestinationRecipient: "0x01"},
		"no recipient": {Token: "USDC", Amount: "1"},
	} {
		if _, err := sendToEvmAction(req, 1); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
