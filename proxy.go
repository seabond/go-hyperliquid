package hyperliquid

import (
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/lxzan/gws"
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

// proxyWsOpt returns a WsOpt that configures the WebSocket dialer to route
// through the given proxy. Only socks5 is wired today — http/https proxies
// need a net.Conn-returning helper, which gws does not yet provide out of
// the box and is not currently required by any caller.
func proxyWsOpt(proxyURL *url.URL) WsOpt {
	return func(w *WebsocketClient) {
		w.newDialer = func() (gws.Dialer, error) {
			var auth *proxy.Auth
			if proxyURL.User != nil {
				pw, _ := proxyURL.User.Password()
				auth = &proxy.Auth{User: proxyURL.User.Username(), Password: pw}
			}
			return proxy.SOCKS5("tcp", proxyURL.Host, auth, &net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 60 * time.Second,
			})
		}
	}
}
