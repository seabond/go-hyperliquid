package hyperliquid

import (
	"context"
	"crypto/ecdsa"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

// stubRoundTripper is a minimal http.RoundTripper that always returns the
// same canned 200 response body, regardless of the request. It lets the
// envelope tests below exercise executeAction's response handling without
// recording a cassette or hitting the network.
type stubRoundTripper struct {
	body string
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

// newExchangeForEnvelopeTest builds a minimal Exchange wired to the package's
// real signing path but pointed at TestnetAPIURL. Skips NewExchange so we do
// not have to fake a Meta fetch. Reuses TestnetAPIURL only as a syntactically
// valid base — the http.DefaultTransport swap intercepts the actual request.
func newExchangeForEnvelopeTest(t *testing.T) *Exchange {
	t.Helper()
	pk, err := crypto.HexToECDSA(strings.Repeat("a", 64))
	require.NoError(t, err)
	pub, ok := pk.Public().(*ecdsa.PublicKey)
	require.True(t, ok)
	return &Exchange{
		account:     NewAccount(pk),
		accountAddr: crypto.PubkeyToAddress(*pub).Hex(),
		client:      newClient(TestnetAPIURL),
	}
}

// TestExecuteAction_ErrorEnvelope locks the contract introduced by da9c815:
// when the /exchange endpoint replies with {"status":"err","response":...},
// executeAction returns an error wrapped with "exchange action rejected:"
// before it tries to unmarshal into the caller's result type. This single
// path is shared by every executeAction caller (orders, cancels, leverage,
// margin, transfers), so anchoring it here prevents the envelope branch
// from being silently dropped — which would otherwise surface as a
// confusing "json: cannot unmarshal..." error from the success path
// instead of HL's actual rejection reason.
func TestExecuteAction_ErrorEnvelope(t *testing.T) {
	const errMsg = "User or API Wallet 0xdead does not exist."

	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &stubRoundTripper{
		body: `{"status":"err","response":"` + errMsg + `"}`,
	}

	ex := newExchangeForEnvelopeTest(t)
	var result struct{}
	err := ex.executeAction(context.Background(), map[string]any{"type": "noop"}, &result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exchange action rejected:")
	require.Contains(t, err.Error(), errMsg)
}

// TestExecuteAction_OkEnvelopeUnmarshalsResult covers the happy-path leg of
// the same branch: a {"status":"ok",...} envelope must NOT trip the
// rejection wrapper, and the body must reach jUnmarshal so the caller's
// result type is populated. Together with TestExecuteAction_ErrorEnvelope
// this pins down the err/ok dichotomy added in da9c815.
func TestExecuteAction_OkEnvelopeUnmarshalsResult(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &stubRoundTripper{
		body: `{"status":"ok","response":{"type":"order","data":{"statuses":[]}}}`,
	}

	ex := newExchangeForEnvelopeTest(t)

	// Use an anonymous struct shaped like Hyperliquid's order envelope so
	// the body parses cleanly and we can assert the success-path field
	// landed where executeAction wrote it.
	var result struct {
		Status   string `json:"status"`
		Response struct {
			Type string `json:"type"`
		} `json:"response"`
	}
	err := ex.executeAction(context.Background(), map[string]any{"type": "noop"}, &result)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Status)
	require.Equal(t, "order", result.Response.Type)
}
