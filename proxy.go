package hyperliquid

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
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
// the given proxy for all requests.
//
// For SOCKS5: uses socks5h:// (remote DNS) via x/net/proxy with a custom
// DialContext that sends raw hostnames to the proxy, avoiding local DNS
// resolution failures for targets behind the proxy.
func proxyClientOpt(proxyURL *url.URL) ClientOpt {
	return func(c *client) {
		transport := &http.Transport{
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

		if proxyURL.Scheme == "socks5" {
			// Use x/net/proxy for SOCKS5 with remote DNS resolution.
			// proxy.SOCKS5 resolves DNS locally by default. To get remote
			// DNS (socks5h behavior), we pass a custom resolver as the
			// "forward" dialer that sends unresolved hostnames.
			auth := &proxy.Auth{}
			if proxyURL.User != nil {
				auth.User = proxyURL.User.Username()
				auth.Password, _ = proxyURL.User.Password()
			}
			// Use a direct dialer that does NOT resolve DNS — the SOCKS5
			// server will resolve the hostname.
			forward := &net.Dialer{Timeout: 10 * time.Second}
			socksDialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, forward)
			if err == nil {
				// Wrap to provide DialContext. The key: we pass the original
				// hostname (not a resolved IP) to the SOCKS5 dialer.
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if ctxd, ok := socksDialer.(proxy.ContextDialer); ok {
						return ctxd.DialContext(ctx, network, addr)
					}
					return socksDialer.Dial(network, addr)
				}
			}
		} else {
			// HTTP/HTTPS proxy: use standard Proxy field.
			transport.Proxy = http.ProxyURL(proxyURL)
		}

		c.httpClient = &http.Client{Transport: transport}
	}
}

// proxyWsOpt returns a WsOpt that configures the WebSocket dialer to use
// the given proxy.
func proxyWsOpt(proxyURL *url.URL) WsOpt {
	return func(w *WebsocketClient) {
		dialer := &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		}

		switch proxyURL.Scheme {
		case "socks5":
			auth := &proxy.Auth{}
			if proxyURL.User != nil {
				auth.User = proxyURL.User.Username()
				auth.Password, _ = proxyURL.User.Password()
			}
			socksDialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
			if err == nil {
				if ctxDialer, ok := socksDialer.(proxy.ContextDialer); ok {
					dialer.NetDialContext = ctxDialer.DialContext
				}
			}
		case "http", "https":
			dialer.Proxy = http.ProxyURL(proxyURL)
		}

		w.dialer = dialer
	}
}
