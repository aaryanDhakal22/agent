package mainclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

type Client struct {
	serverURL  string
	apiKey     string
	httpClient *http.Client
	logger     zerolog.Logger
}

func New(serverURL, apiKey string, logger zerolog.Logger) *Client {
	return &Client{
		serverURL:  strings.TrimRight(serverURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		logger:     logger.With().Str("module", "main-client").Logger(),
	}
}

// FetchRecentOrders fetches the newest `num` orders from the main server.
func (c *Client) FetchRecentOrders(num int) ([]order.OrderRequest, error) {
	url := fmt.Sprintf("%s/api/v1/orders?offset=0&num=%d", c.serverURL, num)
	c.logger.Debug().Str("url", url).Msg("Fetching recent orders from main server")

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch orders: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("main server returned %d", resp.StatusCode)
	}

	var orders []order.OrderRequest
	if err := json.NewDecoder(resp.Body).Decode(&orders); err != nil {
		return nil, fmt.Errorf("decode orders: %w", err)
	}

	c.logger.Debug().Int("count", len(orders)).Msg("Fetched recent orders from main server")
	return orders, nil
}

// FetchOrder fetches a single order by ID from the main server.
func (c *Client) FetchOrder(id int) (*order.OrderRequest, error) {
	url := fmt.Sprintf("%s/api/v1/orders/%d", c.serverURL, id)
	c.logger.Debug().Int("order_id", id).Str("url", url).Msg("Fetching order from main server")

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch order: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("main server returned %d for order %d", resp.StatusCode, id)
	}

	var o order.OrderRequest
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return nil, fmt.Errorf("decode order: %w", err)
	}

	c.logger.Debug().Int("order_id", id).Msg("Fetched order from main server")
	return &o, nil
}
