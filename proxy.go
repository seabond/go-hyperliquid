package hyperliquid

import (
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// ProxyURL parses a proxy URL string and returns both a ClientOpt (for HTTP/REST)
// and a WsOpt (for WebSocket) that route traffic through the proxy.
//
// Supported schemes: http, https, socks5.
//
// Usage:
//
//	clientOpt, wsOpt := hyperliquid.ProxyURL("socks5://user:pass@host:port")
//	info := hyperliquid.NewInfo(ctx, baseURL, false, nil, nil, nil, hyperliquid.InfoOptClientOptions(clientOpt))
//	ws := hyperliquid.NewWebsocketClient(baseURL, wsOpt)
func ProxyURL(rawURL string) (ClientOpt, WsOpt) {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic("hyperliquid.ProxyURL: invalid URL: " + err.Error())
	}

	clientOpt := proxyClientOpt(u)
	wsOpt := proxyWsOpt(u)
	return clientOpt, wsOpt
}

// proxyClientOpt returns a ClientOpt that configures the HTTP client to use
// the given proxy for all requests. Supports http, https, and socks5 schemes
// via the standard library's http.Transport.Proxy.
func proxyClientOpt(proxyURL *url.URL) ClientOpt {
	return func(c *client) {
		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		c.httpClient = &http.Client{Transport: transport}
	}
}

// proxyWsOpt returns a WsOpt that configures the WebSocket dialer to use
// the given proxy.
func proxyWsOpt(proxyURL *url.URL) WsOpt {
	return func(w *WebsocketClient) {
		w.dialer = &websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			Proxy:            http.ProxyURL(proxyURL),
		}
	}
}
