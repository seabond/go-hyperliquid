package hyperliquid

import (
	"io"

	jsoniter "github.com/json-iterator/go"
)

// japi is a drop-in replacement for encoding/json's top-level functions,
// backed by jsoniter with standard-library-compatible settings.
// Struct field types (json.RawMessage, json.Number, etc.) remain from
// encoding/json so that easyjson-generated code is unaffected.
var japi = jsoniter.ConfigCompatibleWithStandardLibrary

func jMarshal(v any) ([]byte, error)                           { return japi.Marshal(v) }
func jUnmarshal(data []byte, v any) error                      { return japi.Unmarshal(data, v) }
func jMarshalIndent(v any, prefix, indent string) ([]byte, error) { return japi.MarshalIndent(v, prefix, indent) }
func jNewDecoder(r io.Reader) *jsoniter.Decoder                { return japi.NewDecoder(r) }
func jValid(data []byte) bool                                  { return japi.Valid(data) }
