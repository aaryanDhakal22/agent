package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Notifier struct {
	appToken    string
	userKey     string
	pushoverURL string
	httpClient  *http.Client
}

type NotifierError struct {
	User    string   `json:"user"`
	Errors  []string `json:"errors"`
	Status  int      `json:"status"`
	Request string   `json:"request"`
}

func NewNotifier(appToken string, userKey string) *Notifier {
	return &Notifier{
		appToken:    appToken,
		userKey:     userKey,
		pushoverURL: "https://api.pushover.net/1/messages.json",
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
}

// Send posts a Pushover notification. The ctx controls cancellation and
// provides the parent span for the outgoing HTTP call; otelhttp will create
// a child HTTP-client span automatically.
func (n *Notifier) Send(ctx context.Context, message, sound string) error {
	form := url.Values{
		"token":   {n.appToken},
		"user":    {n.userKey},
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
