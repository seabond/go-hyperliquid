package hyperliquid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxzan/gws"
	"github.com/stretchr/testify/require"
)

// testServerDiscard ignores all inbound frames server-side. Used by the
// timeout test to keep the TCP link alive (so TCP-level keepalives pass)
// without sending any application-layer data back to the client.
type testServerDiscard struct{ gws.BuiltinEventHandler }

func (testServerDiscard) OnMessage(c *gws.Conn, m *gws.Message) { _ = m.Close() }

// Override OnPing so the server does NOT reply with a Pong frame. The
// client's readWatchdog fires on silence; an auto-pong would reset
// lastReadNanos and defeat the test.
func (testServerDiscard) OnPing(c *gws.Conn, payload []byte) {}

func TestNewWebsocketClient(t *testing.T) {
	require.PanicsWithValue(t,
		"baseURL must have a scheme set, either wss or ws",
		func() { _ = NewWebsocketClient("foobar.com") },
	)

	require.NotPanics(t,
		func() { _ = NewWebsocketClient(MainnetAPIURL) },
		"Mainnet should always work",
	)

	require.NotPanics(t,
		func() { _ = NewWebsocketClient("") },
		"empty URL should default to Mainnet",
	)
}

func TestWsOptReadTimeout(t *testing.T) {
	client := NewWebsocketClient(MainnetAPIURL, WsOptReadTimeout(42*time.Second))
	require.Equal(t, 42*time.Second, client.readTimeout)
}

// TestReadPumpReconnectsOnPeerClose covers the peer-close → reconnect path
// through pingPump's safety net: readPump's error branch does NOT set
// shouldReconnect for non-timeout errors (clean-close / EOF / RST bail
// quietly), so the reconnect has to come from pingPump writing onto the
// dead conn, failing, and calling w.reconnect(ctx).
//
// Regression guard: if pingPump ever stops calling w.reconnect on write
// failure, a peer-initiated close leaves the subscriber permanently
// dataless — exactly the prod incident observed 2026-04-24 when the
// initial hardening pass moved the reconnect out of pingPump without
// adding it to readPump.
func TestReadPumpReconnectsOnPeerClose(t *testing.T) {
	upgrader := gws.NewUpgrader(gws.BuiltinEventHandler{}, nil)
	var connectCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		connectCount.Add(1)
		// Send a clean close frame and drop the TCP conn. Mimics HL
		// sending a 1000/1001 close and closing the socket.
		_ = conn.WriteClose(1000, []byte("bye"))
		_ = conn.NetConn().Close()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := NewWebsocketClient(server.URL,
		WsOptReadTimeout(5*time.Second), // longer than the test window → not the trigger
		WsOptPingInterval(100*time.Millisecond),
	)
	require.NoError(t, client.Connect(ctx))

	// Give pingPump several ticks; each one writes, fails, calls reconnect.
	time.Sleep(2 * time.Second)

	require.GreaterOrEqual(t, int(connectCount.Load()), 2,
		"peer-initiated close must trigger a reconnect via pingPump safety net")

	require.NoError(t, client.Close())
}

// TestReadPumpReconnectsOnTimeout spins up a WebSocket server that accepts
// connections but never sends a message. The client's readWatchdog should
// time out on silence, close the conn, and the readPump defer drives the
// reconnect — yielding at least two upgrades in the 2s test window.
func TestReadPumpReconnectsOnTimeout(t *testing.T) {
	upgrader := gws.NewUpgrader(testServerDiscard{}, nil)
	var connectCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		connectCount.Add(1)
		// Block on ReadLoop to keep the TCP link alive and drain whatever
		// the client writes (subscribes, pings). The discard handler sends
		// nothing back, so the client's read-silence watchdog fires.
		conn.ReadLoop()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 200 ms timeout gives us several watchdog → reconnect cycles within 2s.
	client := NewWebsocketClient(server.URL, WsOptReadTimeout(200*time.Millisecond))
	require.NoError(t, client.Connect(ctx))

	// Allow enough wall-clock time for multiple timeout → reconnect cycles.
	time.Sleep(2 * time.Second)

	require.GreaterOrEqual(t, int(connectCount.Load()), 2,
		"client should have reconnected at least once after read timeout")

	require.NoError(t, client.Close())
}
