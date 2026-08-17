package hyperliquid

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// SendToEvmWithDataPrimaryType is the EIP-712 primary type of the
// sendToEvmWithData action.
const SendToEvmWithDataPrimaryType = "HyperliquidTransaction:SendToEvmWithData"

// sendToEvmWithDataTypes is the action's signed field list.
//
// EIP-712 hashes fields in declaration order, so this slice is the contract:
// reordering it produces a signature Hyperliquid rejects, and adding a field
// the exchange does not expect does the same. The list is transcribed from
// Circle's published integration for the action.
var sendToEvmWithDataTypes = []apitypes.Type{
	{Name: "hyperliquidChain", Type: "string"},
	{Name: "token", Type: "string"},
	{Name: "amount", Type: "string"},
	{Name: "sourceDex", Type: "string"},
	{Name: "destinationRecipient", Type: "string"},
	{Name: "addressEncoding", Type: "string"},
	{Name: "destinationChainId", Type: "uint32"},
	{Name: "gasLimit", Type: "uint64"},
	{Name: "data", Type: "bytes"},
	{Name: "nonce", Type: "uint64"},
}

// Address encodings sendToEvmWithData accepts for its destination recipient.
const (
	AddressEncodingHex    = "hex"
	AddressEncodingBase58 = "base58"
)

// defaultSendToEvmGasLimit bounds the destination-side execution when a
// request leaves GasLimit at zero. It matches Circle's reference integration.
const defaultSendToEvmGasLimit = 200_000

// SendToEvmRequest describes one sendToEvmWithData action: a HyperCore
// withdrawal that leaves the chain through the token's linked contract.
type SendToEvmRequest struct {
	// Token is the token name, e.g. "USDC". Unlike spotSend this action takes
	// the bare name rather than name:id.
	Token string

	// Amount is the exact decimal amount as a string, e.g. "12.5". It is a
	// string rather than a float64 because it is signed verbatim: the value
	// the wire carries and the value the signature covers must be the same
	// text, and a float64 cannot promise that for every amount.
	Amount string

	// SourceDex names the perp dex the funds come from: "" for the default
	// perp dex, "spot" for the spot balance.
	SourceDex string

	// DestinationRecipient receives the funds on the destination chain, in
	// AddressEncoding's form.
	DestinationRecipient string

	// AddressEncoding is AddressEncodingHex or AddressEncodingBase58. Empty
	// means hex.
	AddressEncoding string

	// DestinationDomain is Circle's CCTP domain id for the destination chain
	// — 0 for Ethereum, 3 for Arbitrum, 7 for Polygon. It is NOT an EVM chain
	// id, and passing one sends the funds to a different chain than intended.
	// The wire field is named destinationChainId, which is why this comment
	// exists.
	DestinationDomain uint32

	// GasLimit bounds the destination-side execution. Zero uses
	// defaultSendToEvmGasLimit.
	GasLimit uint64

	// Data is the payload handed to the linked contract. Empty leaves the
	// contract free to attach its own default behaviour — for USDC that is a
	// forwarding hook that has the destination mint delivered automatically.
	Data []byte
}

// SendToEvmWithData withdraws a token from HyperCore to another chain.
//
// Instead of the plain transfer a spot send to a system address performs, the
// linked contract's coreReceiveWithData is called with this action's payload,
// which is how a HyperCore balance reaches a chain that is not HyperEVM. The
// action is user-signed, so it can only move the signing account's own funds.
//
// The sender pays the destination-side gas out of its HyperCore HYPE, or, when
// it holds none, as the USDC equivalent — an account without HYPE still
// completes.
func (e *Exchange) SendToEvmWithData(ctx context.Context, req SendToEvmRequest) (*TransferResponse, error) {
	nonce := e.nextNonce()
	action, err := sendToEvmAction(req, nonce)
	if err != nil {
		return nil, err
	}

	sig, err := e.signUserSignedAction(
		ctx,
		action,
		sendToEvmWithDataTypes,
		SendToEvmWithDataPrimaryType,
		e.client.baseURL == MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := e.postAction(ctx, action, sig, nonce)
	if err != nil {
		return nil, err
	}

	var result TransferResponse
	if err := jUnmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// sendToEvmAction builds the action map for one request. It is separate from
// the send so a test can hash exactly what the exchange would be asked to
// verify, without a network round trip.
func sendToEvmAction(req SendToEvmRequest, nonce int64) (map[string]any, error) {
	if req.Token == "" {
		return nil, errors.New("hyperliquid: send to evm requires a token")
	}
	if req.Amount == "" {
		return nil, errors.New("hyperliquid: send to evm requires an amount")
	}
	if req.DestinationRecipient == "" {
		return nil, errors.New("hyperliquid: send to evm requires a destination recipient")
	}
	encoding := req.AddressEncoding
	if encoding == "" {
		encoding = AddressEncodingHex
	}
	gasLimit := req.GasLimit
	if gasLimit == 0 {
		gasLimit = defaultSendToEvmGasLimit
	}

	return map[string]any{
		"type":                 "sendToEvmWithData",
		"token":                req.Token,
		"amount":               req.Amount,
		"sourceDex":            req.SourceDex,
		"destinationRecipient": req.DestinationRecipient,
		"addressEncoding":      encoding,
		"destinationChainId":   big.NewInt(int64(req.DestinationDomain)),
		"gasLimit":             new(big.Int).SetUint64(gasLimit),
		"data":                 hexutil.Encode(req.Data),
		"nonce":                big.NewInt(nonce),
	}, nil
}
