package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

const (
	initialBackoff = 2 * time.Second
	maxBackoff     = 60 * time.Second
)

type Client struct {
	serverURL string
	apiKey    string
	service   *orderApp.Service
	logger    zerolog.Logger
}

func New(serverURL, apiKey string, svc *orderApp.Service, logger zerolog.Logger) *Client {
	return &Client{
		serverURL: serverURL,
		apiKey:    apiKey,
		service:   svc,
		logger:    logger.With().Str("module", "sse-client").Logger(),
	}
}

// Start connects to the main server SSE stream and blocks until ctx is cancelled.
// It reconnects with exponential backoff on any connection failure or EOF.
func (c *Client) Start(ctx context.Context) {
	c.logger.Info().Str("url", c.serverURL).Msg("Starting SSE client")

	backoff := initialBackoff
	for {
		if err := c.connect(ctx); err != nil {
			if ctx.Err() != nil {
				c.logger.Info().Msg("SSE client stopped")
				return
			}
			c.logger.Error().Err(err).Dur("retry_in", backoff).Msg("SSE connection lost, reconnecting")
		} else {
			c.logger.Info().Msg("SSE connection closed cleanly, reconnecting")
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.logger.Info().Msg("SSE client stopped")
			return
		}

		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	url := strings.TrimRight(c.serverURL, "/") + "/api/v1/events/orders"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from SSE endpoint", resp.StatusCode)
	}

	c.logger.Info().Msg("Connected to main server SSE stream")
	// Reset backoff is handled in Start() after a successful return (nil error).
	return c.readEvents(ctx, resp.Body)
}

func (c *Client) readEvents(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)

	var eventType, dataLine string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Blank line = end of SSE block, dispatch if we have a full event.
			if eventType == "order" && dataLine != "" {
				c.handleOrderEvent(dataLine)
			}
			eventType = ""
			dataLine = ""
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		// Comments (": ...") and id: lines are intentionally ignored.
	}

	return scanner.Err()
}

func (c *Client) handleOrderEvent(data string) {
	var o order.OrderRequest
	if err := json.Unmarshal([]byte(data), &o); err != nil {
		c.logger.Error().Err(err).Str("raw", data).Msg("failed to unmarshal SSE order event")
		return
	}

	c.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer", o.Customer.FirstName+" "+o.Customer.LastName).
		Str("service_type", o.ServiceType).
		Msg("Order received via SSE")

	if err := c.service.Handle(o); err != nil {
		c.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Failed to handle SSE order")
	}
}
