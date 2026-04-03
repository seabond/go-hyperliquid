// Package hyperliquid provides a Go client library for the Hyperliquid exchange API.
// It includes support for both REST API and WebSocket connections, allowing users to
// access market data, manage orders, and handle user account operations.
package hyperliquid

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sethvargo/go-retry"
	"github.com/sonirico/vago/lol"
)

const (
	MainnetAPIURL = "https://api.hyperliquid.xyz"
	TestnetAPIURL = "https://api.hyperliquid-testnet.xyz"
	LocalAPIURL   = "http://localhost:3001"

	// httpErrorStatusCode is the minimum status code considered an error
	httpErrorStatusCode = 400
)

type client struct {
	logger     lol.Logger
	debug      bool
	baseURL    string
	httpClient *http.Client
}

func newClient(baseURL string, opts ...ClientOpt) *client {
	if baseURL == "" {
		baseURL = MainnetAPIURL
	}

	cli := &client{
		baseURL:    baseURL,
		httpClient: new(http.Client),
	}

	for _, opt := range opts {
		opt.Apply(cli)
	}

	return cli
}

func (c *client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	jsonData, err := jMarshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := c.baseURL + path

	b := retry.NewExponential(200 * time.Millisecond)
	b = retry.WithMaxRetries(3, b)
	b = retry.WithCappedDuration(2*time.Second, b)

	var body []byte
	err = retry.Do(ctx, b, func(ctx context.Context) error {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonData))
		if reqErr != nil {
			return fmt.Errorf("failed to create request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")

		if c.debug {
			c.logger.WithFields(lol.Fields{
				"method": "POST",
				"url":    url,
				"body":   string(jsonData),
			}).Debug("HTTP request")
		}

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			return retry.RetryableError(fmt.Errorf("request failed: %w", doErr))
		}
		defer func() { _ = resp.Body.Close() }()

		body = nil
		if resp.Body != nil {
			body, doErr = io.ReadAll(resp.Body)
			if doErr != nil {
				return retry.RetryableError(fmt.Errorf("failed to read response body: %w", doErr))
			}
		}

		if c.debug {
			c.logger.WithFields(lol.Fields{
				"status": resp.Status,
				"body":   string(body),
			}).Debug("HTTP response")
		}

		// 5xx = server error, retryable.
		if resp.StatusCode >= 500 {
			return retry.RetryableError(fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
		}

		// 4xx = client error, not retryable.
		if resp.StatusCode >= httpErrorStatusCode {
			if !jValid(body) {
				return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
			}
			var apiErr APIError
			if unmarshalErr := jUnmarshal(body, &apiErr); unmarshalErr != nil {
				return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
			}
			return apiErr
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return body, nil
}
