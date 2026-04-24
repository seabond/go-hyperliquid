package hyperliquid

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReconnectDelayJitter asserts that reconnectDelay spreads calls within
// the configured ±25% band around w.reconnectWait. Without jitter, N clients
// that hiccuped together would retry in lockstep and thunder on the proxy.
// We sample repeatedly and verify at least one sample falls above the base
// and at least one below, and every sample stays inside the band.
func TestReconnectDelayJitter(t *testing.T) {
	w := &WebsocketClient{reconnectWait: 100 * time.Millisecond}

	base := float64(w.reconnectWait)
	lower := base * (1 - reconnectJitterFactor)
	upper := base * (1 + reconnectJitterFactor)

	const samples = 200
	var sawBelow, sawAbove bool
	for range samples {
		d := float64(w.reconnectDelay())
		require.GreaterOrEqualf(t, d, 0.0, "jittered delay must not go negative")
		require.GreaterOrEqualf(t, d, math.Floor(lower)-1,
			"sample = %v below lower bound %v", d, lower)
		require.LessOrEqualf(t, d, math.Ceil(upper)+1,
			"sample = %v above upper bound %v", d, upper)
		if d < base {
			sawBelow = true
		}
		if d > base {
			sawAbove = true
		}
	}
	require.True(t, sawBelow, "no sample fell below base; jitter looks one-sided")
	require.True(t, sawAbove, "no sample rose above base; jitter looks one-sided")
}

// TestReconnectDelayNeverNegative guards the rand.Float64*2-1 arithmetic:
// with a very small reconnectWait and worst-case factor we must still
// produce a non-negative duration.
func TestReconnectDelayNeverNegative(t *testing.T) {
	w := &WebsocketClient{reconnectWait: 0}
	for range 100 {
		require.GreaterOrEqual(t, int64(w.reconnectDelay()), int64(0),
			"zero base must stay non-negative under jitter")
	}
}
