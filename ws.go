package hyperliquid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
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

	// wsReadTimeout is the default maximum duration to wait for a single read from
	// the server before treating the connection as stalled. Must exceed pingInterval
	// so that normal pong responses do not trigger a false timeout.
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

// rawInboxSize is the buffer between readPump and decodePump. Sized for ~50s
// of burst traffic at 20 msg/s per connection, so a slow callback cannot
// backpressure network reads in steady state.
const rawInboxSize = 1024

type Subscription struct {
	ID      string
	Payload any
	Close   func()
}

type WebsocketClient struct {
	url                   string
	conn                  *websocket.Conn
	dialer                *websocket.Dialer
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
	postIDCounter atomic.Int64            // monotonically increasing request ID
	postInflight  sync.Map                // id (int64) -> chan postResponse
	postSem       chan struct{}            // counting semaphore, cap = maxPostInflight

	// rawInbox decouples network reads from decode/dispatch. readPump pushes
	// raw message bytes; decodePump drains, unmarshals, and fans out to
	// subscribers. A slow callback on one subscription no longer blocks
	// ws reads for the whole connection.
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
			ChannelOpenOrders: NewMsgDispatcher[OpenOrders](ChannelOpenOrders),
			ChannelTwapStates: NewMsgDispatcher[TwapStates](ChannelTwapStates),
			ChannelWebData3:                      NewMsgDispatcher[WebData3](ChannelWebData3),
			ChannelAllDexsClearinghouseState:     NewMsgDispatcher[AllDexsClearinghouseState](ChannelAllDexsClearinghouseState),
			ChannelSpotState:                    NewMsgDispatcher[SpotStateMessage](ChannelSpotState),
			ChannelAllDexsAssetCtxs:             NewMsgDispatcher[AllDexsAssetCtxs](ChannelAllDexsAssetCtxs),
		},
	}

	for _, opt := range opts {
		opt.Apply(cli)
	}

	return cli
}

func (w *WebsocketClient) Connect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		return nil
	}

	if w.dialer == nil {
		w.dialer = websocket.DefaultDialer
	}

	//nolint:bodyclose // WebSocket connections don't have response bodies to close
	conn, _, err := w.dialer.DialContext(ctx, w.url, nil)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	w.conn = conn

	w.reconnectWait = time.Second

	hook := w.onConnect
	reconnected := w.hasConnected
	w.hasConnected = true

	// decodePump is client-scoped (persists across reconnects); readPump and
	// pingPump are connection-scoped and restart on each Connect. Pass the
	// freshly-dialled conn in so each pump operates on *its* conn even if
	// a later reconnect replaces w.conn — their defers must not close or
	// nil a newer conn owned by a younger pump pair.
	w.decodePumpOnce.Do(func() { go w.decodePump() })

	go w.readPump(ctx, conn)
	go w.pingPump(ctx, conn)

	if err = w.resubscribeAll(); err != nil {
		// A partially-sent resubscribe leaves the conn alive but the
		// subscription list incomplete. If we left w.conn set, the next
		// Connect call (from our caller's reconnect loop) would early-
		// return at `if w.conn != nil` and never retry the missing
		// subscribes — silent subscription loss. Roll the conn back so
		// the retry dials fresh and rebuilds the full list.
		_ = conn.Close()
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
// for the correlated response. reqType is "action" or "info". payload is the
// request body — the same map that would go to HTTP /exchange. The returned
// json.RawMessage contains the response payload (identical to HTTP response body).
//
// Returns ErrWsNotConnected if the WS connection is nil.
// Returns ErrInflightFull if maxPostInflight requests are already in-flight.
func (w *WebsocketClient) Post(ctx context.Context, reqType string, payload any) (json.RawMessage, error) {
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

	// 5. Build and send the post message.
	msg := map[string]any{
		"method": "post",
		"id":     id,
		"request": map[string]any{
			"type":    reqType,
			"payload": payload,
		},
	}
	if err := w.writeJSON(msg); err != nil {
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
	// The data envelope: {"id": N, "response": {"type": "action"|"error", "payload": ...}}
	var envelope struct {
		ID       int64 `json:"id"`
		Response struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"response"`
	}
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

	if envelope.Response.Type == "error" {
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

	if w.conn != nil {
		return w.conn.Close()
	}

	for _, subscriber := range w.subscribers {
		subscriber.clear()
	}
	return nil
}

// Private methods

func (w *WebsocketClient) readPump(ctx context.Context, conn *websocket.Conn) {
	shouldReconnect := false
	defer func() {
		// Always close *our* conn — this is the one this goroutine was
		// spawned for. Do not close w.conn blindly: a successful
		// reconnect may already have put a newer conn there.
		_ = conn.Close()

		// Nil w.conn only if it still points at our conn. Otherwise a
		// younger pump pair owns it and we must not touch it.
		w.mu.Lock()
		if w.conn == conn {
			w.conn = nil
		}
		w.mu.Unlock()

		// Fail all pending WS post requests so callers don't hang.
		w.failAllInflight(ErrWsNotConnected)

		if shouldReconnect {
			w.reconnect(ctx)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		default:
			if err := conn.SetReadDeadline(time.Now().Add(w.readTimeout)); err != nil {
				w.logErrf("websocket set read deadline: %v", err)
				return
			}

			_, msg, err := conn.ReadMessage()
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					w.logErrf("websocket read timeout, reconnecting")
					shouldReconnect = true
					return
				}
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					w.logErrf("websocket read error: %v", err)
				}
				return
			}

			if w.debug {
				w.logDebugf("[<] %s", string(msg))
			}

			// Hand off to decodePump. Block if the inbox is full: applying
			// backpressure to the TCP stream is safer than dropping messages.
			select {
			case w.rawInbox <- msg:
			case <-ctx.Done():
				return
			case <-w.done:
				return
			}
		}
	}
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

func (w *WebsocketClient) pingPump(ctx context.Context, conn *websocket.Conn) {
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
				// Close *our* conn so readPump exits on its next read
				// and runs the reconnect loop exactly once. Calling
				// w.reconnect directly from here used to race with
				// readPump's defer triggering a second concurrent
				// reconnect (harmless but wasteful).
				_ = conn.Close()
				return
			}
		}
	}
}

func (w *WebsocketClient) ForceReconnect(ctx context.Context) {
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
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

	return w.conn.WriteJSON(v)
}

// writeConnJSON writes to a caller-supplied conn instead of w.conn. Used
// by pingPump so it continues to ping *its* conn regardless of any
// reconnect that replaced w.conn in the meantime — otherwise a late ping
// could travel over a younger connection and delay detection of its own
// conn's death.
func (w *WebsocketClient) writeConnJSON(conn *websocket.Conn, v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	if w.debug {
		bts, _ := jMarshal(v)
		w.logDebugf("[>] %s", string(bts))
	}

	return conn.WriteJSON(v)
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
