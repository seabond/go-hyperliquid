package hyperliquid

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/vmihailenco/msgpack/v5"
)

func addressToBytes(address string) []byte {
	address = strings.TrimPrefix(address, "0x")
	bytes, _ := hex.DecodeString(address)
	return bytes
}

// convertStr16ToStr8 converts msgpack str16 (0xda + 2 byte length) to str8 (0xd9 + 1 byte length)
// for strings <256 bytes to match Python msgpack behavior.
// Uses a structure-aware msgpack walker to avoid corrupting non-string data
// that happens to contain 0xda as a data byte (e.g. inside uint64 values).
func convertStr16ToStr8(data []byte) []byte {
	result := make([]byte, 0, len(data))
	pos := 0
	for pos < len(data) {
		consumed := walkMsgpackValue(data, pos, &result)
		if consumed <= 0 {
			// Malformed data: copy remaining bytes as-is (fail-safe)
			result = append(result, data[pos:]...)
			break
		}
		pos += consumed
	}
	return result
}

// walkMsgpackValue parses one msgpack value at data[pos], appends the
// (possibly converted) bytes to *result, and returns the number of bytes
// consumed from data. Returns 0 if the data is truncated/malformed.
func walkMsgpackValue(data []byte, pos int, result *[]byte) int {
	if pos >= len(data) {
		return 0
	}
	b := data[pos]
	remaining := len(data) - pos

	// --- Fixed-length single-byte types ---
	// positive fixint 0x00-0x7f, negative fixint 0xe0-0xff, nil 0xc0, never used 0xc1, bool 0xc2-0xc3
	if b <= 0x7f || b >= 0xe0 || (b >= 0xc0 && b <= 0xc3) {
		*result = append(*result, b)
		return 1
	}

	// --- fixstr (0xa0-0xbf): 1 header + N data bytes ---
	if b >= 0xa0 && b <= 0xbf {
		n := int(b & 0x1f)
		total := 1 + n
		if remaining < total {
			return 0
		}
		*result = append(*result, data[pos:pos+total]...)
		return total
	}

	// --- fixmap (0x80-0x8f): N key-value pairs ---
	if b >= 0x80 && b <= 0x8f {
		count := int(b & 0x0f)
		*result = append(*result, b)
		consumed := 1
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	}

	// --- fixarray (0x90-0x9f): N elements ---
	if b >= 0x90 && b <= 0x9f {
		count := int(b & 0x0f)
		*result = append(*result, b)
		consumed := 1
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	}

	switch b {
	// --- float32, float64 ---
	case 0xca: // float32: 1+4
		return copyFixed(data, pos, 5, result)
	case 0xcb: // float64: 1+8
		return copyFixed(data, pos, 9, result)

	// --- unsigned integers ---
	case 0xcc: // uint8: 1+1
		return copyFixed(data, pos, 2, result)
	case 0xcd: // uint16: 1+2
		return copyFixed(data, pos, 3, result)
	case 0xce: // uint32: 1+4
		return copyFixed(data, pos, 5, result)
	case 0xcf: // uint64: 1+8
		return copyFixed(data, pos, 9, result)

	// --- signed integers ---
	case 0xd0: // int8: 1+1
		return copyFixed(data, pos, 2, result)
	case 0xd1: // int16: 1+2
		return copyFixed(data, pos, 3, result)
	case 0xd2: // int32: 1+4
		return copyFixed(data, pos, 5, result)
	case 0xd3: // int64: 1+8
		return copyFixed(data, pos, 9, result)

	// --- fixext 1/2/4/8/16 ---
	case 0xd4: // fixext1: 1+1+1
		return copyFixed(data, pos, 3, result)
	case 0xd5: // fixext2: 1+1+2
		return copyFixed(data, pos, 4, result)
	case 0xd6: // fixext4: 1+1+4
		return copyFixed(data, pos, 6, result)
	case 0xd7: // fixext8: 1+1+8
		return copyFixed(data, pos, 10, result)
	case 0xd8: // fixext16: 1+1+16
		return copyFixed(data, pos, 18, result)

	// --- bin 8/16/32 ---
	case 0xc4: // bin8: 1 + 1-byte len + data
		return copyVarLen(data, pos, 1, result)
	case 0xc5: // bin16: 1 + 2-byte len + data
		return copyVarLen(data, pos, 2, result)
	case 0xc6: // bin32: 1 + 4-byte len + data
		return copyVarLen(data, pos, 4, result)

	// --- ext 8/16/32 ---
	case 0xc7: // ext8: 1 + 1-byte len + 1 type + data
		return copyExtVarLen(data, pos, 1, result)
	case 0xc8: // ext16: 1 + 2-byte len + 1 type + data
		return copyExtVarLen(data, pos, 2, result)
	case 0xc9: // ext32: 1 + 4-byte len + 1 type + data
		return copyExtVarLen(data, pos, 4, result)

	// --- str8 (0xd9): already compact, just copy ---
	case 0xd9: // str8: 1 + 1-byte len + data
		return copyVarLen(data, pos, 1, result)

	// --- str16 (0xda): THE conversion target ---
	case 0xda: // str16: 1 + 2-byte len + data
		if remaining < 3 {
			return 0
		}
		length := (int(data[pos+1]) << 8) | int(data[pos+2])
		total := 3 + length
		if remaining < total {
			return 0
		}
		if length < 256 {
			*result = append(*result, 0xd9)
			*result = append(*result, byte(length)) // #nosec G115 -- length is guaranteed < 256 by the if-guard above
			*result = append(*result, data[pos+3:pos+total]...)
		} else {
			*result = append(*result, data[pos:pos+total]...)
		}
		return total

	// --- str32 (0xdb): just copy ---
	case 0xdb: // str32: 1 + 4-byte len + data
		return copyVarLen(data, pos, 4, result)

	// --- array16/32 ---
	case 0xdc: // array16: 1 + 2-byte count
		if remaining < 3 {
			return 0
		}
		count := (int(data[pos+1]) << 8) | int(data[pos+2])
		*result = append(*result, data[pos:pos+3]...)
		consumed := 3
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	case 0xdd: // array32: 1 + 4-byte count
		if remaining < 5 {
			return 0
		}
		count := (int(data[pos+1]) << 24) | (int(data[pos+2]) << 16) | (int(data[pos+3]) << 8) | int(data[pos+4])
		*result = append(*result, data[pos:pos+5]...)
		consumed := 5
		for i := 0; i < count; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed

	// --- map16/32 ---
	case 0xde: // map16: 1 + 2-byte count
		if remaining < 3 {
			return 0
		}
		count := (int(data[pos+1]) << 8) | int(data[pos+2])
		*result = append(*result, data[pos:pos+3]...)
		consumed := 3
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed
	case 0xdf: // map32: 1 + 4-byte count
		if remaining < 5 {
			return 0
		}
		count := (int(data[pos+1]) << 24) | (int(data[pos+2]) << 16) | (int(data[pos+3]) << 8) | int(data[pos+4])
		*result = append(*result, data[pos:pos+5]...)
		consumed := 5
		for i := 0; i < count*2; i++ {
			c := walkMsgpackValue(data, pos+consumed, result)
			if c <= 0 {
				return 0
			}
			consumed += c
		}
		return consumed

	default:
		// Unknown type: copy single byte as fail-safe
		*result = append(*result, b)
		return 1
	}
}

// copyFixed copies exactly `size` bytes from data[pos:] to result.
// Returns 0 if data is truncated.
func copyFixed(data []byte, pos, size int, result *[]byte) int {
	if len(data)-pos < size {
		return 0
	}
	*result = append(*result, data[pos:pos+size]...)
	return size
}

// copyVarLen handles msgpack types with a variable-length data payload:
// header(1) + length(lenBytes) + data(length). Copies as-is.
func copyVarLen(data []byte, pos, lenBytes int, result *[]byte) int {
	headerSize := 1 + lenBytes
	if len(data)-pos < headerSize {
		return 0
	}
	length := readLen(data, pos+1, lenBytes)
	total := headerSize + length
	if len(data)-pos < total {
		return 0
	}
	*result = append(*result, data[pos:pos+total]...)
	return total
}

// copyExtVarLen handles ext types: header(1) + length(lenBytes) + type(1) + data(length).
func copyExtVarLen(data []byte, pos, lenBytes int, result *[]byte) int {
	headerSize := 1 + lenBytes + 1 // format + len + type byte
	if len(data)-pos < headerSize {
		return 0
	}
	length := readLen(data, pos+1, lenBytes)
	total := headerSize + length
	if len(data)-pos < total {
		return 0
	}
	*result = append(*result, data[pos:pos+total]...)
	return total
}

// readLen reads a big-endian unsigned integer of 1, 2, or 4 bytes.
func readLen(data []byte, pos, size int) int {
	switch size {
	case 1:
		return int(data[pos])
	case 2:
		return (int(data[pos]) << 8) | int(data[pos+1])
	case 4:
		return (int(data[pos]) << 24) | (int(data[pos+1]) << 16) | (int(data[pos+2]) << 8) | int(data[pos+3])
	default:
		return 0
	}
}

// maxActionNesting bounds how deep encodeAction will look for maps. An action is
// a handful of levels deep; anything past this is a cycle, which would otherwise
// be found by the encoder recursing until the stack runs out.
const maxActionNesting = 64

// orderableMapTypes are the map types msgpack's SetSortMapKeys actually orders.
// Every other map type falls back to Go's randomised iteration.
var orderableMapTypes = map[reflect.Type]bool{
	reflect.TypeOf(map[string]string{}):      true,
	reflect.TypeOf(map[string]bool{}):        true,
	reflect.TypeOf(map[string]interface{}{}): true,
}

// encodeAction is the one msgpack encoding of an action. Everything that hashes
// an action and everything that hands the bytes to a remote signer goes through
// here.
//
// It has to be one function and it has to be deterministic. actionHash and
// EncodeAction used to encode separately, and for a map-typed action that
// produced DIFFERENT bytes on each call, because Go randomises map iteration
// order. A remote signer was then shown one encoding while a different one was
// hashed and sent — the exact confusion exporting the bytes exists to remove.
func encodeAction(action any) ([]byte, error) {
	if err := checkOrderableMaps(reflect.ValueOf(action), 0); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	// Compact ints, plus the str16-to-str8 rewrite below, are what make these
	// bytes agree with Python's msgpack — which is what Hyperliquid hashes.
	enc.UseCompactInts(true)
	// Sorting applies to Go maps only: structs still encode in declaration order,
	// which is what mirrors an insertion-ordered Python dict. A Go map has no
	// insertion order to mirror, so some total order must be chosen, and
	// lexicographic is the one the wire already uses — encoding/json sorts map
	// keys, so this is the order Hyperliquid re-derives the hash from when it
	// re-encodes the JSON body it received.
	enc.SetSortMapKeys(true)

	if err := enc.Encode(action); err != nil {
		return nil, fmt.Errorf("encode action: %w", err)
	}
	// Convert str16 to str8 for Python compatibility.
	return convertStr16ToStr8(buf.Bytes()), nil
}

// checkOrderableMaps rejects an action holding a map whose keys the encoder
// cannot put in a fixed order, because such an action has no single hash: two
// encodes of the same value produce different bytes, so the signature would be
// over a pre-image nobody — not the exchange, not an auditor — can reproduce.
// Refusing is the only safe answer.
func checkOrderableMaps(v reflect.Value, depth int) error {
	if depth > maxActionNesting {
		return fmt.Errorf("action nests deeper than %d levels", maxActionNesting)
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkOrderableMaps(v.Elem(), depth+1)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkOrderableMaps(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			// Unexported fields are not encoded, so they cannot affect the bytes.
			if !t.Field(i).IsExported() {
				continue
			}
			if err := checkOrderableMaps(v.Field(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if !orderableMapTypes[v.Type()] {
			return fmt.Errorf(
				"action contains a %s: msgpack cannot order its keys, so the action hash would differ between encodes",
				v.Type(),
			)
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := checkOrderableMaps(iter.Value(), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func actionHash(action any, vaultAddress string, nonce int64, expiresAfter *int64) []byte {
	data, err := encodeAction(action)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal action: %v", err))
	}

	// fmt.Printf("🔍 DEBUG actionHash msgpack: %s\n", hex.EncodeToString(data))

	// Add nonce as 8 bytes big endian
	if nonce < 0 {
		panic(fmt.Sprintf("nonce cannot be negative: %d", nonce))
	}
	nonceBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(nonceBytes, uint64(nonce))
	data = append(data, nonceBytes...)

	// Add vault address
	if vaultAddress == "" {
		data = append(data, 0x00)
	} else {
		data = append(data, 0x01)
		data = append(data, addressToBytes(vaultAddress)...)
	}

	// Add expires_after if provided
	if expiresAfter != nil {
		if *expiresAfter < 0 {
			panic(fmt.Sprintf("expiresAfter cannot be negative: %d", *expiresAfter))
		}
		data = append(data, 0x00)
		expiresAfterBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(expiresAfterBytes, uint64(*expiresAfter))
		data = append(data, expiresAfterBytes...)
	}

	// Return keccak256 hash
	hash := crypto.Keccak256(data)
	// fmt.Printf("   Msgpack data: %s\n", hex.EncodeToString(data))
	// fmt.Printf("   Action hash: %s\n", hex.EncodeToString(hash))
	return hash
}

func constructPhantomAgent(hash []byte, isMainnet bool) map[string]any {
	source := "b" // testnet
	if isMainnet {
		source = "a" // mainnet
	}
	return map[string]any{
		"source":       source,
		"connectionId": hash,
	}
}

func l1Payload(phantomAgent map[string]any, isMainnet bool) apitypes.TypedData {
	// Note: chainId is 1337 for both mainnet and testnet - it's just a signing domain identifier
	chainId := math.HexOrDecimal256(*big.NewInt(1337))

	return apitypes.TypedData{
		Domain: apitypes.TypedDataDomain{
			ChainId:           &chainId,
			Name:              "Exchange",
			Version:           "1",
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Types: apitypes.Types{
			"Agent": []apitypes.Type{
				{Name: "source", Type: "string"},
				{Name: "connectionId", Type: "bytes32"},
			},
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
		},
		PrimaryType: "Agent",
		Message:     phantomAgent,
	}
}

// SignatureResult represents the structured signature result
type SignatureResult struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

// L1ActionSigner signs L1 actions (msgpack + phantom agent EIP-712).
// When nil on Exchange, the default ECDSA implementation is used.
type L1ActionSigner interface {
	SignL1Action(
		ctx context.Context,
		account Account,
		action any,
		vaultAddress string,
		timestamp int64,
		expiresAfter *int64,
		isMainnet bool,
	) (*SignatureResult, error)
}

// UserSignedActionSigner signs direct EIP-712 user-signed actions.
// When nil on Exchange, the default ECDSA implementation is used.
type UserSignedActionSigner interface {
	SignUserSignedAction(
		ctx context.Context,
		account Account,
		action map[string]any,
		payloadTypes []apitypes.Type,
		primaryType string,
		isMainnet bool,
	) (*SignatureResult, error)
}

// AgentSigner signs agent approval actions.
// When nil on Exchange, the default ECDSA implementation is used.
type AgentSigner interface {
	SignAgent(
		ctx context.Context,
		account Account,
		agentAddress, agentName string,
		nonce int64,
		isMainnet bool,
	) (*SignatureResult, error)
}

// hashStructLenient is like HashStruct but ignores fields in message that are not in types
// This matches Python's eth_account behavior where extra fields in message are silently ignored
func hashStructLenient(
	typedData apitypes.TypedData,
	primaryType string,
	message map[string]any,
) ([]byte, error) {
	types := typedData.Types[primaryType]

	// Filter message to only include fields that exist in type definition
	// Also convert numeric types to ensure proper type handling for EIP-712
	filteredMessage := make(map[string]any)
	for _, t := range types {
		if val, ok := message[t.Name]; ok {
			// Convert numeric types to ensure proper type handling for EIP-712
			// apitypes.HashStruct expects specific types based on the type declaration
			switch t.Type {
			case "uint64":
				var uintVal uint64
				switch v := val.(type) {
				case uint64:
					uintVal = v
				case int64:
					if v < 0 {
						return nil, fmt.Errorf("cannot convert negative int64 %d to uint64", v)
					}
					uintVal = uint64(v)
				case float64:
					// JSON unmarshaling can convert numbers to float64
					if v < 0 || v > float64(^uint64(0)) || v != float64(uint64(v)) {
						return nil, fmt.Errorf("invalid float64 value %f for uint64", v)
					}
					uintVal = uint64(v)
				case int:
					if v < 0 {
						return nil, fmt.Errorf("cannot convert negative int %d to uint64", v)
					}
					uintVal = uint64(v)
				case json.Number:
					// Handle json.Number type
					parsed, err := strconv.ParseUint(string(v), 10, 64)
					if err != nil {
						return nil, fmt.Errorf(
							"failed to parse json.Number %s to uint64 for %s: %w",
							v,
							t.Name,
							err,
						)
					}
					uintVal = parsed
				case string:
					// Try to parse as string representation of uint64
					parsed, err := strconv.ParseUint(v, 10, 64)
					if err != nil {
						return nil, fmt.Errorf(
							"failed to parse string %s to uint64 for %s: %w",
							v,
							t.Name,
							err,
						)
					}
					uintVal = parsed
				default:
					// Try to convert via json marshal/unmarshal to handle edge cases
					jsonBytes, err := jMarshal(v)
					if err != nil {
						return nil, fmt.Errorf("failed to marshal value for %s: %w", t.Name, err)
					}
					if err := jUnmarshal(jsonBytes, &uintVal); err != nil {
						return nil, fmt.Errorf(
							"failed to convert value to uint64 for %s: %w",
							t.Name,
							err,
						)
					}
				}
				// apitypes.HashStruct may not handle uint64 directly from map[string]any
				// Convert to *big.Int which is commonly used for EIP-712 uint types
				filteredMessage[t.Name] = new(big.Int).SetUint64(uintVal)
			default:
				filteredMessage[t.Name] = val
			}
		}
	}

	// Now use standard HashStruct with filtered message
	return typedData.HashStruct(primaryType, filteredMessage)
}

func signInner(
	privateKey *ecdsa.PrivateKey,
	typedData apitypes.TypedData,
) (SignatureResult, error) {
	// Create EIP-712 hash
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to hash domain: %w", err)
	}

	// Use lenient hashing to allow extra fields in message (Python compatibility)
	typedDataHash, err := hashStructLenient(typedData, typedData.PrimaryType, typedData.Message)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to hash typed data: %w", err)
	}

	rawData := []byte{0x19, 0x01}
	rawData = append(rawData, domainSeparator...)
	rawData = append(rawData, typedDataHash...)
	msgHash := crypto.Keccak256Hash(rawData)

	signature, err := crypto.Sign(msgHash.Bytes(), privateKey)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to sign message: %w", err)
	}

	// Extract r, s, v components
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:64])
	v := int(signature[64]) + 27

	// DEBUG: Verify signature recovery
	// pubKey, err := crypto.SigToPub(msgHash.Bytes(), signature)
	// if err == nil {
	// 	recoveredAddr := crypto.PubkeyToAddress(*pubKey)
	// 	expectedAddr := crypto.PubkeyToAddress(privateKey.PublicKey)
	// 	fmt.Printf("   DEBUG SIGNATURE:\n")
	// 	fmt.Printf("   Expected address: %s\n", expectedAddr.Hex())
	// 	fmt.Printf("   Recovered address: %s\n", recoveredAddr.Hex())
	// 	fmt.Printf("   Match: %v\n", recoveredAddr.Hex() == expectedAddr.Hex())
	// 	fmt.Printf("   msgHash: %s\n", msgHash.Hex())
	//}

	return SignatureResult{
		R: hexutil.EncodeBig(r),
		S: hexutil.EncodeBig(s),
		V: v,
	}, nil
}

// SignUserSignedAction signs actions that require direct EIP-712 signing
// (e.g., approveAgent, approveBuilderFee, convertToMultiSigUser)
//
// IMPORTANT: The message will contain MORE fields than declared in payloadTypes to avoid the error
// "422 Failed to deserialize the JSON body" and "User or API Wallet 0x123... does not exist".
// This matches Python SDK behavior where the field order doesn't matter and extra fields (type, signatureChainId)
// are present in the message but ignored during EIP-712 hashing via hashStructLenient.
func SignUserSignedAction(
	ctx context.Context,
	account Account,
	action map[string]any,
	payloadTypes []apitypes.Type,
	primaryType string,
	isMainnet bool,
) (*SignatureResult, error) {
	// Mutating the caller's map is load-bearing, not a slip: postAction sends this
	// same map as the request body, and Hyperliquid rejects a body without
	// signatureChainId and hyperliquidChain.
	addUserSignedEnvelope(action, isMainnet)

	// signInner uses hashStructLenient which filters message to only include
	// fields declared in payloadTypes, matching Python eth_account behavior
	return account.SignTypedData(ctx, userSignedPayload(action, payloadTypes, primaryType))
}

// AddUserSignedEnvelope adds the two fields every user-signed action carries.
// signatureChainId is the chain the wallet signs on; hyperliquidChain names the
// environment and is what stops a testnet signature being replayed on mainnet.
//
// Exported because a caller that signs through its own signer has to send the
// SAME map the signature commits to. Exchange.postAction sends whatever map it
// was handed, and only SignUserSignedAction added these fields — so a custom
// signer could produce a correct signature over a body it could not then
// assemble, which surfaces as a rejected withdrawal rather than as a missing
// function. It is idempotent, so calling it before handing the action to the SDK
// is safe.
func AddUserSignedEnvelope(action map[string]any, isMainnet bool) {
	addUserSignedEnvelope(action, isMainnet)
}

func addUserSignedEnvelope(action map[string]any, isMainnet bool) {
	action["signatureChainId"] = "0x66eee"
	action["hyperliquidChain"] = "Mainnet"
	if !isMainnet {
		action["hyperliquidChain"] = "Testnet"
	}
}

// userSignedPayload builds the EIP-712 payload for a user-signed action. It is
// the single definition of that payload, so the signing path and the exported
// hashes cannot drift apart.
func userSignedPayload(
	action map[string]any,
	payloadTypes []apitypes.Type,
	primaryType string,
) apitypes.TypedData {
	// Note: chainId is hardcoded to 421614 just like the Python SDK
	chainId := math.HexOrDecimal256(*big.NewInt(421614))
	return apitypes.TypedData{
		Domain: apitypes.TypedDataDomain{
			ChainId:           &chainId,
			Name:              "HyperliquidSignTransaction",
			Version:           "1",
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Types: apitypes.Types{
			primaryType: payloadTypes,
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
		},
		PrimaryType: primaryType,
		Message:     action,
	}
}

// UserSignedActionHashes returns the EIP-712 domain separator and typed-data
// hash for a user-signed action — the two 32-byte values a remote signer is
// asked to sign over.
//
// The user-signed class is the value-moving one: usdSend, withdraw3, sendAsset,
// approveAgent. It has no msgpack and no phantom agent, so L1ActionHashes cannot
// reach it. Without this, a deployment that pushes signing out to a custody
// service can only describe its trading actions, and the transfers — the ones
// worth authorizing — would have to be signed blind or have this domain
// reimplemented from documentation, where being subtly wrong surfaces as a
// rejected withdrawal rather than as an obvious bug.
//
// Unlike SignUserSignedAction this does not modify the caller's map: it is a
// question about an action, not a step in sending one.
func UserSignedActionHashes(
	action map[string]any,
	payloadTypes []apitypes.Type,
	primaryType string,
	isMainnet bool,
) (domainSeparator, typedDataHash []byte, err error) {
	withEnvelope := make(map[string]any, len(action)+2)
	for k, v := range action {
		withEnvelope[k] = v
	}
	addUserSignedEnvelope(withEnvelope, isMainnet)

	td := userSignedPayload(withEnvelope, payloadTypes, primaryType)
	domainSeparator, err = td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		return nil, nil, fmt.Errorf("hash EIP712Domain: %w", err)
	}
	// hashStructLenient, not HashStruct: the message carries fields payloadTypes
	// does not declare, and the signing path ignores them the same way.
	typedDataHash, err = hashStructLenient(td, td.PrimaryType, td.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("hash %s: %w", td.PrimaryType, err)
	}
	return domainSeparator, typedDataHash, nil
}

func SignL1Action(
	ctx context.Context,
	account Account,
	action any,
	vaultAddress string,
	timestamp int64,
	expiresAfter *int64,
	isMainnet bool,
) (*SignatureResult, error) {
	// Step 1: Create action hash
	hash := actionHash(action, vaultAddress, timestamp, expiresAfter)
	// fmt.Printf("[DEBUG] SignL1Action - ActionHash: %x\n", hash)

	// Step 2: Construct phantom agent
	phantomAgent := constructPhantomAgent(hash, isMainnet)

	// Step 3: Create l1 payload
	typedData := l1Payload(phantomAgent, isMainnet)

	// Step 4: Sign using EIP-712
	return account.SignTypedData(ctx, typedData)
}

type signUsdClassTransferAction struct {
	Type   string  `msgpack:"type"`
	Amount float64 `msgpack:"amount"`
	ToPerp bool    `msgpack:"toPerp"`
}

// SignUsdClassTransferAction signs USD class transfer action
func SignUsdClassTransferAction(
	ctx context.Context,
	account Account,
	amount float64,
	toPerp bool,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signUsdClassTransferAction{
		Type:   "usdClassTransfer",
		Amount: amount,
		ToPerp: toPerp,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signSpotTransferAction struct {
	Type        string  `msgpack:"type"`
	Amount      float64 `msgpack:"amount"`
	Destination string  `msgpack:"destination"`
	Token       string  `msgpack:"token"`
}

// SignSpotTransferAction signs spot transfer action
func SignSpotTransferAction(
	ctx context.Context,
	account Account,
	amount float64,
	destination, token string,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signSpotTransferAction{
		Type:        "spotTransfer",
		Amount:      amount,
		Destination: destination,
		Token:       token,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signUsdTransferAction struct {
	Type        string  `msgpack:"type"`
	Amount      float64 `msgpack:"amount"`
	Destination string  `msgpack:"destination"`
}

// SignUsdTransferAction signs USD transfer action
func SignUsdTransferAction(
	ctx context.Context,
	account Account,
	amount float64,
	destination string,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signUsdTransferAction{
		Type:        "usdTransfer",
		Amount:      amount,
		Destination: destination,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signPerpDexClassTransferAction struct {
	Type   string  `msgpack:"type"`
	Dex    string  `msgpack:"dex"`
	Token  string  `msgpack:"token"`
	Amount float64 `msgpack:"amount"`
	ToPerp bool    `msgpack:"toPerp"`
}

// SignPerpDexClassTransferAction signs perp dex class transfer action
func SignPerpDexClassTransferAction(
	ctx context.Context,
	account Account,
	dex, token string,
	amount float64,
	toPerp bool,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signPerpDexClassTransferAction{
		Type:   "perpDexClassTransfer",
		Dex:    dex,
		Token:  token,
		Amount: amount,
		ToPerp: toPerp,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signTokenDelegateAction struct {
	Type             string  `msgpack:"type"`
	Token            string  `msgpack:"token"`
	Amount           float64 `msgpack:"amount"`
	ValidatorAddress string  `msgpack:"validatorAddress"`
}

// SignTokenDelegateAction signs token delegate action
func SignTokenDelegateAction(
	ctx context.Context,
	account Account,
	token string,
	amount float64,
	validatorAddress string,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signTokenDelegateAction{
		Type:             "tokenDelegate",
		Token:            token,
		Amount:           amount,
		ValidatorAddress: validatorAddress,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signWithdrawFromBridgeAction struct {
	Type        string  `msgpack:"type"`
	Destination string  `msgpack:"destination"`
	Amount      float64 `msgpack:"amount"`
	Fee         float64 `msgpack:"fee"`
}

// SignWithdrawFromBridgeAction signs withdraw from bridge action
func SignWithdrawFromBridgeAction(
	ctx context.Context,
	account Account,
	destination string,
	amount, fee float64,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signWithdrawFromBridgeAction{
		Type:        "withdrawFromBridge",
		Destination: destination,
		Amount:      amount,
		Fee:         fee,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

// SignAgent signs agent approval action using EIP-712 direct signing
func SignAgent(
	ctx context.Context,
	account Account,
	agentAddress, agentName string,
	nonce int64,
	isMainnet bool,
) (*SignatureResult, error) {
	// The nonce must be non-negative
	if nonce < 0 {
		return nil, fmt.Errorf("nonce cannot be negative: %d", nonce)
	}

	// Use int64 in the action map - apitypes will handle the conversion to uint64
	// based on the type declaration in payloadTypes
	action := map[string]any{
		"type":         "approveAgent",
		"agentAddress": agentAddress,
		"agentName":    agentName,
		"nonce":        nonce,
	}

	// payload_types from Python: only declares fields that are in the original action
	// signatureChainId and hyperliquidChain are added by SignUserSignedAction
	// but they're NOT declared in payloadTypes (they're added to message dynamically)
	payloadTypes := []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "agentAddress", Type: "address"},
		{Name: "agentName", Type: "string"},
		{Name: "nonce", Type: "uint64"},
	}

	return SignUserSignedAction(
		ctx,
		account,
		action,
		payloadTypes,
		"HyperliquidTransaction:ApproveAgent",
		isMainnet,
	)
}

type signApproveBuilderFee struct {
	Type string `msgpack:"type"`
	// BuilderAddress is the address of the builder
	BuilderAddress string `msgpack:"builderAddress"`
	// MaxFeeRate is the maximum fee rate the user is willing to pay
	MaxFeeRate float64 `msgpack:"maxFeeRate"`
}

// SignApproveBuilderFee signs approve builder fee action
func SignApproveBuilderFee(
	ctx context.Context,
	account Account,
	builderAddress string,
	maxFeeRate float64,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signApproveBuilderFee{
		Type:           "approveBuilderFee",
		BuilderAddress: builderAddress,
		MaxFeeRate:     maxFeeRate,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signConvertToMultiSigUserAction struct {
	Type      string   `msgpack:"type"`
	Signers   []string `msgpack:"signers"`
	Threshold int      `msgpack:"threshold"`
}

// SignConvertToMultiSigUserAction signs convert to multi-sig user action
func SignConvertToMultiSigUserAction(
	ctx context.Context,
	account Account,
	signers []string,
	threshold int,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signConvertToMultiSigUserAction{
		Type:      "convertToMultiSigUser",
		Signers:   signers,
		Threshold: threshold,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

type signMultiSigAction struct {
	Type       string         `msgpack:"type"`
	Action     map[string]any `msgpack:"action"`
	Signers    []string       `msgpack:"signers"`
	Signatures []string       `msgpack:"signatures"`
}

// SignMultiSigAction signs multi-signature action
func SignMultiSigAction(
	ctx context.Context,
	account Account,
	innerAction map[string]any,
	signers []string,
	signatures []string,
	timestamp int64,
	isMainnet bool,
) (*SignatureResult, error) {
	action := signMultiSigAction{
		Type:       "multiSig",
		Action:     innerAction,
		Signers:    signers,
		Signatures: signatures,
	}

	return SignL1Action(ctx, account, action, "", timestamp, nil, isMainnet)
}

// FloatToUsdInt converts float to USD integer representation
func FloatToUsdInt(value float64) int {
	// Convert float USD to integer representation (assuming 6 decimals for USDC)
	return int(value * 1e6)
}

// GetTimestampMs returns current timestamp in milliseconds
func GetTimestampMs() int64 {
	return time.Now().UnixMilli()
}

// ── exported so a remote signer can be shown what it is signing ──

// EncodeAction returns the exact msgpack bytes this SDK hashes and sends.
//
// It exists so an action can be handed to a remote signer alongside the request
// to sign it. Without this the signer receives only a domain separator and a
// typed-data hash, which makes a limit order and a bridge withdrawal
// indistinguishable — so "may sign" means "may sign anything", and every risk
// control has to live in the caller that a compromise would bypass.
//
// The bytes must come from HERE rather than from the signer's own encoder. The
// request body Hyperliquid receives is encoded by this SDK; a signer that
// re-encoded and got different bytes would produce a signature that does not
// match the body, and the exchange would reject it. Handing over these exact
// bytes also means the signer needs no encoder of its own, and no agreement
// about msgpack's fiddlier corners — declaration-ordered struct fields, compact
// ints, the str16-to-str8 rewrite for Python compatibility.
//
// It shares encodeAction with actionHash rather than re-implementing it. Those
// two were once separate copies of the same eight lines, and they silently
// disagreed for map-typed actions; see encodeAction.
func EncodeAction(action any) ([]byte, error) {
	return encodeAction(action)
}

// ActionHash returns keccak256(msgpack(action) || nonce || vault flag [|| expiresAfter]),
// the value the phantom agent commits to.
//
// Exported for the same reason as EncodeAction: a remote signer, or an auditor,
// can check that a description of an action really produces the hash being
// signed.
func ActionHash(action any, vaultAddress string, nonce int64, expiresAfter *int64) []byte {
	return actionHash(action, vaultAddress, nonce, expiresAfter)
}

// L1ActionPayload returns the EIP-712 payload this SDK signs for an L1 action.
//
// A custom L1ActionSigner receives the raw action and must produce a signature
// over exactly what the default path would have signed. Without this it would
// have to rebuild the phantom agent and the Exchange domain by hand, and any
// divergence produces a signature Hyperliquid rejects — a failure that surfaces
// as a rejected order rather than as an obvious bug.
//
// Pair it with EncodeAction when the signer is remote: EncodeAction gives the
// bytes the action hashes to, and this gives the payload those bytes commit to.
func L1ActionPayload(action any, vaultAddress string, nonce int64, expiresAfter *int64, isMainnet bool) apitypes.TypedData {
	hash := actionHash(action, vaultAddress, nonce, expiresAfter)
	return l1Payload(constructPhantomAgent(hash, isMainnet), isMainnet)
}

// L1ActionHashes returns the EIP-712 domain separator and typed-data hash for an
// L1 action — the two 32-byte values a remote signer is asked to sign over.
func L1ActionHashes(action any, vaultAddress string, nonce int64, expiresAfter *int64, isMainnet bool) (domainSeparator, typedDataHash []byte, err error) {
	td := L1ActionPayload(action, vaultAddress, nonce, expiresAfter, isMainnet)
	domainSeparator, err = td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		return nil, nil, fmt.Errorf("hash EIP712Domain: %w", err)
	}
	typedDataHash, err = td.HashStruct(td.PrimaryType, td.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("hash %s: %w", td.PrimaryType, err)
	}
	return domainSeparator, typedDataHash, nil
}
