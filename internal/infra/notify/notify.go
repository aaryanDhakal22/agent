package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Notifier sends Pushover notifications to two audiences: the owner
// (userKey — everything) and, optionally, the restaurant's boss (bossKey —
// floor problems only). See Send vs SendFloor.
type Notifier struct {
	mu          sync.RWMutex
	appToken    string
	userKey     string
	bossKey     string
	pushoverURL string
	httpClient  *http.Client
}

type NotifierError struct {
	User    string   `json:"user"`
	Errors  []string `json:"errors"`
	Status  int      `json:"status"`
	Request string   `json:"request"`
}

func NewNotifier(appToken, userKey, bossKey string) *Notifier {
	return &Notifier{
		appToken:    appToken,
		userKey:     userKey,
		bossKey:     bossKey,
		pushoverURL: "https://api.pushover.net/1/messages.json",
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
}

// SetKeys swaps the Pushover app token + recipients at runtime (a manager-
// login link hands them over; no restart needed).
func (n *Notifier) SetKeys(appToken, userKey, bossKey string) {
	n.mu.Lock()
	n.appToken, n.userKey, n.bossKey = appToken, userKey, bossKey
	n.mu.Unlock()
}

// Configured reports whether Send/SendFloor would page anyone.
func (n *Notifier) Configured() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.appToken != "" && n.userKey != ""
}

// Send notifies the owner only — system events (agent started, sync trouble).
// The ctx controls cancellation and provides the parent span for the
// outgoing HTTP call; otelhttp creates a child HTTP-client span.
func (n *Notifier) Send(ctx context.Context, message, sound string) error {
	n.mu.RLock()
	user := n.userKey
	n.mu.RUnlock()
	return n.post(ctx, user, message, sound)
}

// SendFloor notifies the owner AND the boss (when PUSHOVER_BOSS_KEY is set) —
// problems on the restaurant floor: an order nobody accepted, nothing printed,
// a rerouted ticket, unmatched items, a printer down / back up.
func (n *Notifier) SendFloor(ctx context.Context, message, sound string) error {
	var errs []error
	for _, key := range n.floorKeys() {
		if err := n.post(ctx, key, message, sound); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) floorKeys() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	keys := []string{n.userKey}
	if n.bossKey != "" && n.bossKey != n.userKey {
		keys = append(keys, n.bossKey)
	}
	return keys
}

func (n *Notifier) post(ctx context.Context, user, message, sound string) error {
	n.mu.RLock()
	token := n.appToken
	n.mu.RUnlock()
	if token == "" || user == "" {
		return nil // not configured (yet): an unlinked agent has no keys
	}
	form := url.Values{
		"token":   {token},
		"user":    {user},
		"message": {message},
		"sound":   {sound},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.pushoverURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("pushover: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushover: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var errData NotifierError
		if jsonErr := json.Unmarshal(body, &errData); jsonErr == nil && errData.Status != 0 {
			return fmt.Errorf("pushover error: %+v", errData)
		}
		return fmt.Errorf("pushover returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
