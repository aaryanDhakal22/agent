// Package mainclient is the agent's HTTP client to main/. It backs token
// provisioning (a device-authed verify GET), the historical-orders view, the
// store-config editor, and self-revoke — all proxied so the browser stays
// same-origin and the store key never leaves the Pi. Outbound requests are
// otelhttp-traced so the agent→main hop shows up in Tempo.
package mainclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// storeAPIKeyHeader is the header main/ authenticates the agent with. Matches
// main/'s middleware.StoreAPIKeyHeader.
const storeAPIKeyHeader = "X-STORE-API-KEY"

// Client talks to a single main/ base URL.
type Client struct {
	baseURL string
	http    *http.Client
	// onUnauthorized, if set, is fired (async) whenever main answers 401 to a
	// device-keyed request — a definitive "this key is revoked/unknown". The
	// agent wires it to wipe local provisioning so ANY denied request (the
	// heartbeat, history, store-config, RPC replies) resets it to the setup
	// screen, not just an SSE reconnect. Receives the key that was rejected so
	// the handler can ignore a stale 401 for a since-rotated token.
	onUnauthorized func(key string)
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
}

// SetOnUnauthorized registers the 401 hook (see the field doc). Set once at wiring
// time, after the SSE controller exists.
func (c *Client) SetOnUnauthorized(f func(key string)) { c.onUnauthorized = f }

// downloadTimeout bounds a large download (a tablet-app APK over the
// restaurant's internet).
const downloadTimeout = 5 * time.Minute

// Download is Get for large bodies (an APK): same auth + 401 hook, longer
// timeout.
func (c *Client) Download(ctx context.Context, key, path, rawQuery string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	return c.doWith(&http.Client{Transport: c.http.Transport}, ctx, http.MethodGet, key, path, rawQuery, nil)
}

// Upload POSTs a large binary body (a nightly SQLite backup) with the long
// download timeout.
func (c *Client) Upload(ctx context.Context, key, path string, body []byte, contentType string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	return c.doWithType(&http.Client{Transport: c.http.Transport}, ctx, http.MethodPost, key, path, "", body, contentType)
}

// Get proxies an authenticated GET to main, returning the upstream status and
// raw body so the agent handler can relay both verbatim. path is a main-side
// path like "/api/v1/orders"; rawQuery is the already-encoded query string.
func (c *Client) Get(ctx context.Context, key, path, rawQuery string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, key, path, rawQuery, nil)
}

// Post proxies an authenticated POST to main with a JSON body.
func (c *Client) Post(ctx context.Context, key, path string, body []byte) (int, []byte, error) {
	return c.do(ctx, http.MethodPost, key, path, "", body)
}

// PostRPC posts a duplex-channel envelope to main's /store/v1/agent/rpc using
// the store key.
func (c *Client) PostRPC(ctx context.Context, key string, body []byte) (int, []byte, error) {
	return c.Post(ctx, key, "/store/v1/agent/rpc", body)
}

// RevokeSelf asks main to revoke the device this key belongs to (agent Settings →
// "Revoke my token"). Returns the HTTP status so the caller can treat an
// already-invalid key (401) as "already gone" rather than a failure.
func (c *Client) RevokeSelf(ctx context.Context, key string) (int, error) {
	status, _, err := c.do(ctx, http.MethodPost, key, "/store/v1/agent/revoke", "", nil)
	return status, err
}

func (c *Client) do(ctx context.Context, method, key, path, rawQuery string, body []byte) (int, []byte, error) {
	return c.doWith(c.http, ctx, method, key, path, rawQuery, body)
}

func (c *Client) doWith(hc *http.Client, ctx context.Context, method, key, path, rawQuery string, body []byte) (int, []byte, error) {
	return c.doWithType(hc, ctx, method, key, path, rawQuery, body, "application/json")
}

func (c *Client) doWithType(hc *http.Client, ctx context.Context, method, key, path, rawQuery string, body []byte, contentType string) (int, []byte, error) {
	url := c.baseURL + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(storeAPIKeyHeader, key)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := hc.Do(req)
	if err != nil {
		// Transport/connection failure (main unreachable) — NOT a 401, so it never
		// triggers a wipe. Callers surface it as "connecting"/bad-gateway.
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// A 401 from any device-keyed request means the key is revoked/unknown — fire
	// the hook (async, non-blocking) so the agent resets to the setup screen.
	if resp.StatusCode == http.StatusUnauthorized && c.onUnauthorized != nil && key != "" {
		go c.onUnauthorized(key)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
