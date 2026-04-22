package sse

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/store"

	"github.com/rs/zerolog"
)

const (
	initialBackoff  = 2 * time.Second
	maxBackoff      = 60 * time.Second
	backlogFetchNum = 50
)

// MainClient fetches recent orders from the main server for backlog recovery.
type MainClient interface {
	FetchRecentOrders(num int) ([]order.OrderRequest, error)
}

// http11Client forces HTTP/1.1 by disabling HTTP/2 negotiation (TLS ALPN).
// SSE requires a persistent connection — HTTP/2 multiplexed streams are
// terminated by proxies on idle timeout, causing spurious disconnects.
var http11Client = &http.Client{
	Transport: &http.Transport{
		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	},
}

type Client struct {
	serverURL      string
	apiKey         string
	service        *orderApp.Service
	store          *store.Store
	mainClient     MainClient
	logger         zerolog.Logger
	reconnectCount int
}

func New(serverURL, apiKey string, svc *orderApp.Service, st *store.Store, mc MainClient, logger zerolog.Logger) *Client {
	return &Client{
		serverURL:  serverURL,
		apiKey:     apiKey,
		service:    svc,
		store:      st,
		mainClient: mc,
		logger:     logger.With().Str("module", "sse-client").Logger(),
	}
}

// Start connects to the main server SSE stream and blocks until ctx is cancelled.
// It reconnects with exponential backoff on any failure.
func (c *Client) Start(ctx context.Context) {
	c.logger.Info().
		Str("server_url", c.serverURL).
		Msg("SSE client starting")

	backoff := initialBackoff

	for {
		c.logger.Info().
			Int("attempt", c.reconnectCount+1).
			Dur("backoff", backoff).
			Msg("Connecting to main server SSE stream")

		err := c.connect(ctx)

		if ctx.Err() != nil {
			c.logger.Info().Msg("SSE client stopped — context cancelled")
			return
		}

		if err != nil {
			c.logger.Error().
				Err(err).
				Int("reconnect_count", c.reconnectCount).
				Dur("retry_in", backoff).
				Msg("SSE connection failed, will retry")
		} else {
			c.logger.Warn().
				Int("reconnect_count", c.reconnectCount).
				Dur("retry_in", backoff).
				Msg("SSE stream closed cleanly by server, will reconnect")
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.logger.Info().Msg("SSE client stopped — context cancelled during backoff")
			return
		}

		if backoff < maxBackoff {
			backoff *= 2
		}
		c.reconnectCount++
	}
}

func (c *Client) connect(ctx context.Context) error {
	url := strings.TrimRight(c.serverURL, "/") + "/api/v1/events/orders"

	c.logger.Debug().Str("url", url).Msg("Building SSE request")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-API-Key", c.apiKey)

	c.logger.Debug().Str("url", url).Msg("Dialing main server (HTTP/1.1 forced)")

	resp, err := http11Client.Do(req)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from SSE endpoint", resp.StatusCode)
	}

	c.logger.Info().
		Str("url", url).
		Int("reconnect_count", c.reconnectCount).
		Msg("Connected to main server SSE stream")

	// On any reconnect (not the very first connection), fetch and print backlogged orders.
	if c.reconnectCount > 0 {
		c.logger.Info().Msg("Reconnected — fetching backlogged orders from main server")
		c.printBacklog(ctx)
	} else {
		c.logger.Info().Msg("First connection established — skipping backlog (no prior session)")
	}

	return c.readEvents(ctx, resp.Body)
}

// printBacklog fetches recent orders from the main server and prints any that
// arrived while the agent was disconnected.
func (c *Client) printBacklog(ctx context.Context) {
	// Use the highest order ID in the store as a session watermark.
	// If the store is empty we have no reference point for this session
	// (agent may have restarted or disconnected before receiving any orders),
	// so skip to avoid reprinting orders from a previous session.
	entries := c.store.List()
	if len(entries) == 0 {
		c.logger.Info().Msg("Store empty — skipping backlog (no session watermark to compare against)")
		return
	}

	highestKnownID := 0
	for _, e := range entries {
		if e.Order.OrderID > highestKnownID {
			highestKnownID = e.Order.OrderID
		}
	}
	c.logger.Debug().
		Int("highest_known_id", highestKnownID).
		Int("fetch_num", backlogFetchNum).
		Msg("Session watermark set — fetching recent orders for backlog check")

	orders, err := c.mainClient.FetchRecentOrders(backlogFetchNum)
	if err != nil {
		c.logger.Error().Err(err).Msg("Failed to fetch backlog from main server — skipping backlog print")
		return
	}

	c.logger.Debug().Int("fetched", len(orders)).Msg("Backlog orders fetched from main server")

	// Only print orders strictly newer than our watermark — anything at or
	// below highestKnownID was either already printed or predates this session.
	var missed []order.OrderRequest
	for _, o := range orders {
		if o.OrderID > highestKnownID {
			missed = append(missed, o)
		}
	}

	if len(missed) == 0 {
		c.logger.Info().Msg("No backlogged orders to print — store is up to date")
		return
	}

	c.logger.Info().
		Int("missed", len(missed)).
		Msg("Backlogged orders found — printing oldest first")

	// Orders from main are newest-first; reverse to print oldest-first.
	for i, j := 0, len(missed)-1; i < j; i, j = i+1, j-1 {
		missed[i], missed[j] = missed[j], missed[i]
	}

	for _, o := range missed {
		select {
		case <-ctx.Done():
			c.logger.Warn().Msg("Context cancelled during backlog print — stopping")
			return
		default:
		}

		c.logger.Info().
			Int("order_id", o.OrderID).
			Str("customer", o.Customer.FirstName+" "+o.Customer.LastName).
			Msg("Printing backlogged order")

		if err := c.service.Handle(o); err != nil {
			c.logger.Error().
				Err(err).
				Int("order_id", o.OrderID).
				Msg("Failed to print backlogged order — skipping")
			continue
		}

		c.store.Add(o)
		c.logger.Info().Int("order_id", o.OrderID).Msg("Backlogged order printed and stored")
	}

	c.logger.Info().Int("printed", len(missed)).Msg("Backlog print complete")
}

func (c *Client) readEvents(ctx context.Context, r io.Reader) error {
	c.logger.Debug().Msg("Starting SSE event loop")

	scanner := bufio.NewScanner(r)
	// Default 64 KB buffer is too small for large order payloads with many items.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // up to 1 MB per line

	var eventType, dataLine string
	lineCount := 0

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			c.logger.Info().Msg("Context cancelled — exiting SSE event loop")
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		lineCount++
		c.logger.Trace().Str("line", line).Int("line_count", lineCount).Msg("SSE raw line received")

		if line == "" {
			if eventType != "" || dataLine != "" {
				c.logger.Debug().
					Str("event_type", eventType).
					Int("data_len", len(dataLine)).
					Msg("SSE block complete — dispatching")

				if eventType == "order" && dataLine != "" {
					c.handleOrderEvent(dataLine)
				} else {
					c.logger.Debug().Str("event_type", eventType).Msg("SSE event ignored (not an order event)")
				}
			}
			eventType = ""
			dataLine = ""
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			c.logger.Debug().Str("event_type", eventType).Msg("SSE event type set")
		} else if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			c.logger.Debug().Int("data_len", len(dataLine)).Msg("SSE data line received")
		} else if strings.HasPrefix(line, ":") {
			c.logger.Debug().Str("comment", line).Msg("SSE comment/keepalive received")
		} else {
			c.logger.Debug().Str("line", line).Msg("SSE unrecognised line — ignoring")
		}
	}

	if err := scanner.Err(); err != nil {
		c.logger.Error().Err(err).Msg("SSE scanner error")
		return err
	}

	c.logger.Info().Int("lines_read", lineCount).Msg("SSE stream reached EOF")
	return nil
}

func (c *Client) handleOrderEvent(data string) {
	c.logger.Debug().Int("data_len", len(data)).Msg("Unmarshalling SSE order event")

	var o order.OrderRequest
	if err := json.Unmarshal([]byte(data), &o); err != nil {
		c.logger.Error().
			Err(err).
			Str("raw_data", data).
			Msg("Failed to unmarshal SSE order event — skipping")
		return
	}

	customerName := o.Customer.FirstName + " " + o.Customer.LastName

	c.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer", customerName).
		Str("service_type", o.ServiceType).
		Float64("total", o.OrderTotal).
		Int("item_count", len(o.Items)).
		Msg("Order received via SSE")

	c.logger.Debug().Int("order_id", o.OrderID).Msg("Dispatching order to handler")

	if err := c.service.Handle(o); err != nil {
		c.logger.Error().
			Err(err).
			Int("order_id", o.OrderID).
			Str("customer", customerName).
			Msg("Failed to handle SSE order")
		return
	}

	c.store.Add(o)

	c.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer", customerName).
		Msg("Order handled and stored successfully")
}
