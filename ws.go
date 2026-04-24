package hyperliquid

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxzan/gws"
	"github.com/sonirico/vago/lol"
	"github.com/sonirico/vago/maps"
)

// reconnectJitterFactor keeps reconnect sleeps inside a ±25% band around
// the exponential-backoff base. When many clients (e.g. a pool of 10 WS
// connections behind one process, or multiple processes behind the same
// proxy IP) disconnect together, un-jittered waits retry in lockstep and
// thunder on the proxy / HL endpoint. 25% is small enough not to extend
// the tail and big enough to spread out a herd within a second.
const reconnectJitterFactor = 0.25

const (
	// pingInterval is the interval for sending ping messages to keep WebSocket alive
	pingInterval = 30 * time.Second

	// wsReadTimeout is the default maximum duration to wait for a single read
	// from the server before treating the connection as stalled. Must exceed
	// pingInterval so that normal pong responses do not trigger a false timeout.
	wsReadTimeout = 90 * time.Second
)

var (
	// ErrInflightFull is returned when the maximum number of concurrent WS post
	// requests (100) has been reached. Callers should fall back to HTTP.
	ErrInflightFull = errors.New("ws post: inflight limit reached")

	// ErrWsNotConnected is returned when Post is called but no WS connection is active.
	ErrWsNotConnected = errors.New("ws post: not connected")
)

// postResponse carries the result of a single WS post request back to the
// waiting caller in Post().
type postResponse struct {
	Data json.RawMessage
	Err  error
}

// maxPostInflight is the Hyperliquid WS post concurrency limit.
const maxPostInflight = 100

// rawInboxSize is the buffer between the gws OnMessage handler and
// decodePump. Sized for ~50s of burst traffic at 20 msg/s per connection,
// so a slow callback cannot backpressure network reads in steady state.
const rawInboxSize = 1024

type Subscription struct {
	ID      string
	Payload any
	Close   func()
}

type WebsocketClient struct {
	url                   string
	conn                  *gws.Conn
	newDialer             func() (gws.Dialer, error) // optional proxy dialer factory
	tlsConfig             *tls.Config                // optional TLS config (nil → system defaults)
	mu                    sync.RWMutex
	writeMu               sync.Mutex
	subscribers           map[string]*uniqSubscriber
	msgDispatcherRegistry map[string]msgDispatcher
	nextSubID             atomic.Int64
	done                  chan struct{}
	closeOnce             sync.Once
	reconnectWait         time.Duration
	readTimeout           time.Duration
	pingEvery             time.Duration
	debug                 bool
	logger                lol.Logger
	onConnect             func(context.Context, bool)
	hasConnected          bool

	// WS post support: send exchange/info actions over WS instead of HTTP.
	postIDCounter atomic.Int64 // monotonically increasing request ID
	postInflight  sync.Map     // id (int64) -> chan postResponse
	postSem       chan struct{} // counting semaphore, cap = maxPostInflight

	// rawInbox decouples network reads from decode/dispatch. The gws
	// OnMessage handler pushes raw message bytes; decodePump drains,
	// unmarshals, and fans out to subscribers. A slow callback on one
	// subscription no longer blocks ws reads for the whole connection.
	rawInbox       chan []byte
	decodePumpOnce sync.Once
}

var upstreamHosts map[string]struct{}

func init() {
	mustHost := func(s string) string {
		u, err := url.Parse(s)
		if err != nil {
			panic(fmt.Sprintf("invalid upstream URL %q: %v", s, err))
		}
		return strings.ToLower(u.Hostname())
	}
	upstreamHosts = map[string]struct{}{
		mustHost(MainnetAPIURL): {},
		mustHost(TestnetAPIURL): {},
	}
}

func isUpstream(u *url.URL) bool {
	_, ok := upstreamHosts[strings.ToLower(u.Hostname())]
	return ok
}

func NewWebsocketClient(baseURL string, opts ...WsOpt) *WebsocketClient {
	if baseURL == "" {
		baseURL = MainnetAPIURL
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		log.Fatalf("invalid URL: %v", err)
	}

	// the current usage expects a full address (https://api.hyp..) to keep compatibility check if
	// that host is set and just use the old method. any new caller with their own endpoint will be
	// forced to provide a full URI
	if isUpstream(parsedURL) {
		parsedURL.Scheme = "wss"
		parsedURL.Path = "/ws"
	} else {
		switch parsedURL.Scheme {
		case "https":
			parsedURL.Scheme = "wss"
		case "http":
			parsedURL.Scheme = "ws"
		case "":
			// baseURL has no scheme set, odd
			panic("baseURL must have a scheme set, either wss or ws")
		}
	}

	wsURL := parsedURL.String()

	cli := &WebsocketClient{
		url:           wsURL,
		done:          make(chan struct{}),
		reconnectWait: time.Second,
		readTimeout:   wsReadTimeout,
		pingEvery:     pingInterval,
		postSem:       make(chan struct{}, maxPostInflight),
		rawInbox:      make(chan []byte, rawInboxSize),
		subscribers:   make(map[string]*uniqSubscriber),
		msgDispatcherRegistry: map[string]msgDispatcher{
			ChannelPong:           NewPongDispatcher(),
			ChannelTrades:         NewMsgDispatcher[Trades](ChannelTrades),
			ChannelActiveAssetCtx: NewMsgDispatcher[ActiveAssetCtx](ChannelActiveAssetCtx),
			ChannelL2Book:         NewMsgDispatcher[L2Book](ChannelL2Book),
			ChannelCandle:         NewMsgDispatcher[Candle](ChannelCandle),
			ChannelAllMids:        NewMsgDispatcher[AllMids](ChannelAllMids),
			ChannelNotification:   NewMsgDispatcher[Notification](ChannelNotification),
			ChannelOrderUpdates:   NewMsgDispatcher[WsOrders](ChannelOrderUpdates),
			ChannelWebData2:       NewMsgDispatcher[WebData2](ChannelWebData2),
			ChannelBbo:            NewMsgDispatcher[Bbo](ChannelBbo),
			ChannelUserFills:      NewMsgDispatcher[WsOrderFills](ChannelUserFills),
			ChannelSubResponse:    NewNoopDispatcher(),
			ChannelClearinghouseState: NewMsgDispatcher[ClearinghouseStateMessage](
				ChannelClearinghouseState,
			),
			ChannelOpenOrders:                NewMsgDispatcher[OpenOrders](ChannelOpenOrders),
			ChannelTwapStates:                NewMsgDispatcher[TwapStates](ChannelTwapStates),
			ChannelWebData3:                  NewMsgDispatcher[WebData3](ChannelWebData3),
			ChannelAllDexsClearinghouseState: NewMsgDispatcher[AllDexsClearinghouseState](ChannelAllDexsClearinghouseState),
			ChannelSpotState:                 NewMsgDispatcher[SpotStateMessage](ChannelSpotState),
			ChannelAllDexsAssetCtxs:          NewMsgDispatcher[AllDexsAssetCtxs](ChannelAllDexsAssetCtxs),
		},
	}

	for _, opt := range opts {
		opt.Apply(cli)
	}

	return cli
}

// eventHandler implements gws.Event. One handler is created per Connect
// cycle and bound to the pumps for that conn generation. Its OnClose
// flags shouldReconnect so the readPump goroutine can drive the
// reconnect loop after ReadLoop returns. lastReadNanos is updated on
// every inbound frame so the watchdog can detect silent connections.
type eventHandler struct {
	w               *WebsocketClient
	shouldReconnect atomic.Bool
	lastReadNanos   atomic.Int64
}

func (h *eventHandler) touchRead() {
	h.lastReadNanos.Store(time.Now().UnixNano())
}

func (h *eventHandler) OnOpen(c *gws.Conn) {
	// Seed the watchdog clock so it doesn't fire on the first tick if
	// the initial subscribe ack takes a beat to arrive.
	h.touchRead()
}

func (h *eventHandler) OnClose(c *gws.Conn, err error) {
	// Any close means this conn is dead. Gate the reconnect on w.done so
	// an explicit Close() doesn't trigger an infinite reconnect loop.
	select {
	case <-h.w.done:
		return
	default:
	}
	if err != nil {
		h.w.logErrf("ws: OnClose %v", err)
	}
	h.shouldReconnect.Store(true)
}

func (h *eventHandler) OnPing(c *gws.Conn, payload []byte) {
	h.touchRead()
	_ = c.WritePong(payload)
}

func (h *eventHandler) OnPong(c *gws.Conn, payload []byte) { h.touchRead() }

func (h *eventHandler) OnMessage(c *gws.Conn, message *gws.Message) {
	defer message.Close()
	h.touchRead()

	// message.Data is a pooled *bytes.Buffer; copy before queueing so
	// decodePump can work on a stable byte slice while gws returns the
	// buffer to its pool.
	src := message.Bytes()
	msg := make([]byte, len(src))
	copy(msg, src)

	if h.w.debug {
		h.w.logDebugf("[<] %s", string(msg))
	}

	select {
	case h.w.rawInbox <- msg:
	case <-h.w.done:
		return
	}
}

func (w *WebsocketClient) Connect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		return nil
	}

	handler := &eventHandler{w: w}
	opt := &gws.ClientOption{
		Addr:             w.url,
		TlsConfig:        w.tlsConfig,
		HandshakeTimeout: 15 * time.Second,
	}
	if w.newDialer != nil {
		opt.NewDialer = w.newDialer
	}

	conn, _, err := gws.NewClient(handler, opt)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	w.conn = conn

	w.reconnectWait = time.Second

	hook := w.onConnect
	reconnected := w.hasConnected
	w.hasConnected = true

	// decodePump is client-scoped (persists across reconnects); readPump
	// and pingPump are connection-scoped and restart on each Connect.
	// Pass the freshly-dialled conn in so each pump operates on *its*
	// conn even if a later reconnect replaces w.conn — their defers
	// must not close or nil a newer conn owned by a younger pump pair.
	w.decodePumpOnce.Do(func() { go w.decodePump() })

	go w.readPump(ctx, conn, handler)
	go w.pingPump(ctx, conn)
	go w.readWatchdog(ctx, conn, handler)

	if err = w.resubscribeAll(); err != nil {
		// A partially-sent resubscribe leaves the conn alive but the
		// subscription list incomplete. If we left w.conn set, the next
		// Connect call (from our caller's reconnect loop) would early-
		// return at `if w.conn != nil` and never retry the missing
		// subscribes — silent subscription loss. Roll the conn back so
		// the retry dials fresh and rebuilds the full list.
		_ = conn.WriteClose(1000, nil)
		w.conn = nil
		return fmt.Errorf("resubscribe failed, connection rolled back: %w", err)
	}

	if hook != nil {
		hook(ctx, reconnected)
	}

	return nil
}

type Handler[T subscriptable] func(wsMessage) (T, error)

func (w *WebsocketClient) subscribe(
	payload subscriptable,
	callback func(any),
) (*Subscription, error) {
	if callback == nil {
		return nil, fmt.Errorf("callback cannot be nil")
	}

	w.mu.Lock()

	pkey := payload.Key()
	subscriber, exists := w.subscribers[pkey]
	if !exists {
		subscriber = newUniqSubscriber(
			pkey,
			payload,
			// on subscribe
			func(p subscriptable) {
				if err := w.sendSubscribe(p); err != nil {
					w.logErrf("failed to subscribe: %v", err)
				}
			},
			// on unsubscribe
			func(p subscriptable) {
				w.mu.Lock()
				defer w.mu.Unlock()
				delete(w.subscribers, pkey)
				if err := w.sendUnsubscribe(p); err != nil {
					w.logErrf("failed to unsubscribe: %v", err)
				}
			},
		)

		w.subscribers[pkey] = subscriber
	}

	w.mu.Unlock()

	nextID := w.nextSubID.Add(1)
	subID := key(pkey, strconv.Itoa(int(nextID)))
	subscriber.subscribe(subID, callback)

	return &Subscription{
		ID: subID,
		Close: func() {
			subscriber.unsubscribe(subID)
		},
	}, nil
}

// Post sends a signed exchange (or info) request over the WebSocket and waits
// for the correlated response. reqType is PostTypeAction or PostTypeInfo.
// payload is the request body — the same struct that would go to HTTP
// /exchange. The returned json.RawMessage contains the response payload
// (identical to HTTP response body).
//
// Returns ErrWsNotConnected if the WS connection is nil.
// Returns ErrInflightFull if maxPostInflight requests are already in-flight.
func (w *WebsocketClient) Post(ctx context.Context, reqType PostType, payload any) (json.RawMessage, error) {
	// 1. Check connection.
	w.mu.RLock()
	connected := w.conn != nil
	w.mu.RUnlock()
	if !connected {
		return nil, ErrWsNotConnected
	}

	// 2. Acquire semaphore (non-blocking).
	select {
	case w.postSem <- struct{}{}:
		defer func() { <-w.postSem }()
	default:
		return nil, ErrInflightFull
	}

	// 3. Generate unique request ID.
	id := w.postIDCounter.Add(1)

	// 4. Register response channel in inflight map.
	ch := make(chan postResponse, 1)
	w.postInflight.Store(id, ch)
	defer w.postInflight.Delete(id)

	// 4a. Close the check→Store atomicity window: if readPump's defer fired
	// between our step 1 conn-check and this Store, failAllInflight's
	// sync.Map.Range snapshot may have happened before our Store and will
	// never signal this id. Re-check w.conn here and bail early if nil so
	// writeJSON doesn't block and the caller doesn't hang until ctx expires.
	w.mu.RLock()
	conn := w.conn
	w.mu.RUnlock()
	if conn == nil {
		return nil, ErrWsNotConnected
	}

	// 5. Build and send the post message.
	cmd := wsPostCommand{
		Method: "post",
		ID:     id,
		Request: wsPostRequest{
			Type:    reqType,
			Payload: payload,
		},
	}
	if err := w.writeJSON(cmd); err != nil {
		return nil, fmt.Errorf("ws post send: %w", err)
	}

	// 6. Wait for correlated response or context cancellation.
	select {
	case resp := <-ch:
		return resp.Data, resp.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, ErrWsNotConnected
	}
}

// failAllInflight sends an error to every pending Post caller. Called when the
// WS connection drops so that waiters don't hang until their context expires.
func (w *WebsocketClient) failAllInflight(err error) {
	w.postInflight.Range(func(key, value any) bool {
		ch := value.(chan postResponse)
		select {
		case ch <- postResponse{Err: err}:
		default:
		}
		return true
	})
}

// dispatchPostResponse handles an incoming "post" channel message from the WS
// read pump. It extracts the request ID, looks up the corresponding inflight
// channel, and delivers the result.
func (w *WebsocketClient) dispatchPostResponse(raw json.RawMessage) {
	var envelope wsPostResponse
	if err := jUnmarshal(raw, &envelope); err != nil {
		w.logErrf("ws post: failed to parse response envelope: %v", err)
		return
	}

	val, ok := w.postInflight.Load(envelope.ID)
	if !ok {
		// No waiter — the caller may have timed out and cleaned up.
		w.logDebugf("ws post: no inflight waiter for id %d", envelope.ID)
		return
	}
	ch := val.(chan postResponse)

	if envelope.Response.Type == PostTypeError {
		// Error responses have a string payload like "400 Bad Request: ...".
		var errMsg string
		if err := jUnmarshal(envelope.Response.Payload, &errMsg); err != nil {
			errMsg = string(envelope.Response.Payload)
		}
		select {
		case ch <- postResponse{Err: fmt.Errorf("ws post error: %s", errMsg)}:
		default:
		}
		return
	}

	// Success — pass the payload through as raw bytes so the caller can
	// unmarshal it the same way it would unmarshal an HTTP response body.
	select {
	case ch <- postResponse{Data: envelope.Response.Payload}:
	default:
	}
}

func (w *WebsocketClient) Close() error {
	var err error
	w.closeOnce.Do(func() {
		err = w.close()
	})
	return err
}

func (w *WebsocketClient) close() error {
	close(w.done)

	// Fail all pending WS post requests immediately.
	w.failAllInflight(ErrWsNotConnected)

	w.mu.Lock()
	defer w.mu.Unlock()

	// Clear subscribers first. Previous layout early-returned on
	// `w.conn != nil` before the subscriber.clear loop, so the normal
	// close path (conn live → Close succeeds → return) never ran the
	// loop. Callback closures in w.subscribers stayed referenced by the
	// client until it went out of scope — small leak, visible in heap
	// profiles during deploy churn.
	for _, subscriber := range w.subscribers {
		subscriber.clear()
	}

	if w.conn != nil {
		// Swallow errors here — an already-torn-down conn returns
		// "use of closed network connection", which isn't actionable
		// during shutdown.
		_ = w.conn.WriteClose(1000, nil)
	}
	return nil
}

// Private methods

func (w *WebsocketClient) readPump(ctx context.Context, conn *gws.Conn, h *eventHandler) {
	defer func() {
		// Always close *our* conn — this is the one this goroutine was
		// spawned for. Do not close w.conn blindly: a successful
		// reconnect may already have put a newer conn there.
		_ = conn.WriteClose(1000, nil)

		// Nil w.conn only if it still points at our conn. Otherwise a
		// younger pump pair owns it and we must not touch it.
		w.mu.Lock()
		if w.conn == conn {
			w.conn = nil
		}
		w.mu.Unlock()

		// Fail all pending WS post requests so callers don't hang.
		w.failAllInflight(ErrWsNotConnected)

		if h.shouldReconnect.Load() {
			w.reconnect(ctx)
		}
	}()

	// gws's ReadLoop blocks, driving OnMessage / OnClose synchronously on
	// this goroutine (until ClientOption.ParallelEnabled is flipped, which
	// we don't). Returns when the conn closes for any reason; OnClose
	// fires just before ReadLoop returns, so shouldReconnect is set by
	// the time our defer runs.
	conn.ReadLoop()
}

// decodePump is the single consumer of rawInbox. It runs for the lifetime
// of the client (started once via decodePumpOnce) and survives reconnects.
func (w *WebsocketClient) decodePump() {
	for {
		select {
		case <-w.done:
			return
		case raw, ok := <-w.rawInbox:
			if !ok {
				return
			}
			var wsMsg wsMessage
			if err := jUnmarshal(raw, &wsMsg); err != nil {
				w.logErrf("websocket message parse error: %v", err)
				continue
			}
			if err := w.dispatch(wsMsg); err != nil {
				w.logErrf("failed to dispatch websocket message: %v", err)
			}
		}
	}
}

func (w *WebsocketClient) pingPump(ctx context.Context, conn *gws.Conn) {
	ticker := time.NewTicker(w.pingEvery)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.writeConnJSON(conn, wsCommand{Method: "ping"}); err != nil {
				w.logErrf("ping error: %v", err)
				// Close *our* conn so ReadLoop exits on its next frame
				// read and readPump's defer triggers the reconnect
				// cleanly (and without touching a newer w.conn a racing
				// reconnect may have put there — see the
				// `if w.conn == conn` gate in readPump's defer).
				// Connect is idempotent (early-returns on w.conn != nil)
				// so even if readPump's defer also reaches reconnect
				// the duplicate call is a no-op.
				_ = conn.WriteClose(1000, nil)
				w.reconnect(ctx)
				return
			}
		}
	}
}

// readWatchdog fires when no inbound frame (any channel, including app-level
// pong) has arrived within w.readTimeout. gws's ReadLoop does not auto-extend
// a read deadline the way gorilla's SetReadDeadline-per-read pattern did, so
// without this goroutine a silent-but-TCP-alive conn would never be detected
// (pingPump's write-failure safety net only catches TCP death). Firing means:
// close the conn so ReadLoop returns → OnClose sets shouldReconnect →
// readPump's defer drives the reconnect.
//
// Check cadence is readTimeout/4 — tight enough that the detected-silence
// window stays close to readTimeout, loose enough to avoid wake-up overhead
// on idle connections. The atomic read of lastReadNanos keeps the hot path
// (OnMessage) lock-free.
func (w *WebsocketClient) readWatchdog(ctx context.Context, conn *gws.Conn, h *eventHandler) {
	if w.readTimeout <= 0 {
		return
	}
	interval := w.readTimeout / 4
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, h.lastReadNanos.Load())
			if time.Since(last) > w.readTimeout {
				w.logErrf("ws: read watchdog: no inbound for %s, reconnecting", w.readTimeout)
				// Close this pump's conn; swallow errors (a racing
				// reconnect may have closed it already). The readPump
				// goroutine will see OnClose, set shouldReconnect via
				// its handler, exit ReadLoop, and drive the reconnect
				// in its defer.
				_ = conn.WriteClose(1000, nil)
				return
			}
		}
	}
}

func (w *WebsocketClient) ForceReconnect(ctx context.Context) {
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.WriteClose(1000, nil)
		w.conn = nil
	}
	w.mu.Unlock()

	go w.reconnect(ctx)
}

func (w *WebsocketClient) dispatch(msg wsMessage) error {
	// WS post responses arrive on channel "post" — they are correlated by ID,
	// not dispatched through the subscription registry.
	if msg.Channel == "post" {
		w.dispatchPostResponse(msg.Data)
		return nil
	}

	dispatcher, ok := w.msgDispatcherRegistry[msg.Channel]
	if !ok {
		return fmt.Errorf("no dispatcher for channel: %s", msg.Channel)
	}

	w.mu.RLock()
	subscribers := maps.Values(w.subscribers)
	w.mu.RUnlock()

	return dispatcher.Dispatch(subscribers, msg)
}

func (w *WebsocketClient) reconnect(ctx context.Context) {
	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		default:
			if err := w.Connect(ctx); err == nil {
				return
			}
			time.Sleep(w.reconnectDelay())
			w.reconnectWait *= 2
			if w.reconnectWait > time.Minute {
				w.reconnectWait = time.Minute
			}
		}
	}
}

// reconnectDelay returns the current exponential-backoff base with a ±25%
// jitter applied. Spreads a thundering herd of simultaneously-disconnected
// clients so they don't retry in lockstep.
func (w *WebsocketClient) reconnectDelay() time.Duration {
	base := float64(w.reconnectWait)
	jitter := (rand.Float64()*2 - 1) * base * reconnectJitterFactor
	d := time.Duration(base + jitter)
	if d < 0 {
		d = 0
	}
	return d
}

func (w *WebsocketClient) resubscribeAll() error {
	for _, subscriber := range w.subscribers {
		if err := w.sendSubscribe(subscriber.subscriptionPayload); err != nil {
			return fmt.Errorf("resubscribe: %w", err)
		}
	}
	return nil
}

func (w *WebsocketClient) sendSubscribe(payload subscriptable) error {
	return w.writeJSON(wsCommand{
		Method:       "subscribe",
		Subscription: payload,
	})
}

func (w *WebsocketClient) sendUnsubscribe(payload subscriptable) error {
	return w.writeJSON(wsCommand{
		Method:       "unsubscribe",
		Subscription: payload,
	})
}

func (w *WebsocketClient) writeJSON(v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	if w.conn == nil {
		return fmt.Errorf("connection closed")
	}

	if w.debug {
		bts, _ := jMarshal(v)
		w.logDebugf("[>] %s", string(bts))
	}

	b, err := jMarshal(v)
	if err != nil {
		return err
	}
	return w.conn.WriteMessage(gws.OpcodeText, b)
}

// writeConnJSON writes to a caller-supplied conn instead of w.conn. Used
// by pingPump so it continues to ping *its* conn regardless of any
// reconnect that replaced w.conn in the meantime — otherwise a late ping
// could travel over a younger connection and delay detection of its own
// conn's death.
func (w *WebsocketClient) writeConnJSON(conn *gws.Conn, v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	if w.debug {
		bts, _ := jMarshal(v)
		w.logDebugf("[>] %s", string(bts))
	}

	b, err := jMarshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(gws.OpcodeText, b)
}

func (w *WebsocketClient) logErrf(fmt string, args ...any) {
	if w.logger == nil {
		return
	}

	w.logger.Errorf(fmt, args...)
}

func (w *WebsocketClient) logDebugf(fmt string, args ...any) {
	if w.logger == nil {
		return
	}

	w.logger.Debugf(fmt, args...)
}
