package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Notifier sends Pushover notifications. One account (userKey) carries the
// admin's phones, the store owner's phones and the drivers' phones, told
// apart by Pushover device name (2.3.7):
//
//   - Send (system events) → the admin devices;
//   - SendFloor (floor problems) → the admin devices (2.5.2: no longer the
//     owner: the owner's alerts go through SendOwner);
//   - SendOwner → the store owner's devices + the boss key (a separate
//     account, no device), only what application/owneralert lets through
//     (contract §2.1e "Owner alerts");
//   - SendRoute → one driver's device.
//
// With no admin devices known nothing is scoped: Send and SendFloor go to
// every device on the account (the behaviour before 2.3.7).
type Notifier struct {
	mu           sync.RWMutex
	appToken     string
	userKey      string
	bossKey      string
	adminDevices []string
	ownerDevices []string
	// strictScope (2.4.1, every APP_ENV but prod): with no admin devices,
	// Send and SendFloor don't send at all — drivers' phones are devices on
	// the same account, and a dev or staging agent must never page them.
	strictScope bool
	pushoverURL string
	httpClient  *http.Client
}

type NotifierError struct {
	User    string   `json:"user"`
	Errors  []string `json:"errors"`
	Status  int      `json:"status"`
	Request string   `json:"request"`
}

// ErrNotConfigured: no app token or user key (an unlinked agent).
var ErrNotConfigured = errors.New("pushover: not configured")

// APIError is Pushover refusing a message (HTTP 4xx with its error list).
type APIError struct {
	Status int
	Errors []string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pushover returned %d: %s", e.Status, strings.Join(e.Errors, "; "))
}

// DeviceUnknown reports Pushover's "device name is not valid for user".
func (e *APIError) DeviceUnknown() bool {
	for _, m := range e.Errors {
		if strings.Contains(strings.ToLower(m), "device") {
			return true
		}
	}
	return false
}

func NewNotifier(appToken, userKey, bossKey string) *Notifier {
	u := os.Getenv("PUSHOVER_API_URL") // tests only: a fake Pushover
	if u == "" {
		u = "https://api.pushover.net/1/messages.json"
	}
	return &Notifier{
		appToken:    appToken,
		userKey:     userKey,
		bossKey:     bossKey,
		pushoverURL: u,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
}

var deviceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,25}$`)

// ValidDevice is Pushover's device-name rule: 1-25 of [A-Za-z0-9_-].
func ValidDevice(name string) bool { return deviceRe.MatchString(name) }

// ParseDevices splits a comma list ("AaryanPhone, tablet"), trimmed and
// de-duplicated; names Pushover would refuse are dropped.
func ParseDevices(list string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range strings.Split(list, ",") {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] || !ValidDevice(d) {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// SetAdminDevices sets the admin's phones (Doppler, handed over by main).
func (n *Notifier) SetAdminDevices(devices []string) {
	n.mu.Lock()
	n.adminDevices = devices
	n.mu.Unlock()
}

// SetOwnerDevices sets the store owner's phones (store_config).
func (n *Notifier) SetOwnerDevices(devices []string) {
	n.mu.Lock()
	n.ownerDevices = devices
	n.mu.Unlock()
}

// AdminScoped reports whether alerts are limited to named devices.
func (n *Notifier) AdminScoped() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.adminDevices) > 0
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

// SetStrictScope: no admin devices → no Send / SendFloor (outside prod).
func (n *Notifier) SetStrictScope(strict bool) {
	n.mu.Lock()
	n.strictScope = strict
	n.mu.Unlock()
}

// Send notifies the owner only — system events (agent started, sync trouble).
// The ctx controls cancellation and provides the parent span for the
// outgoing HTTP call; otelhttp creates a child HTTP-client span.
func (n *Notifier) Send(ctx context.Context, message, sound string) error {
	n.mu.RLock()
	m := Message{User: n.userKey, Devices: n.adminDevices, Text: message, Sound: sound}
	skip := n.strictScope && len(n.adminDevices) == 0
	n.mu.RUnlock()
	if skip {
		return nil
	}
	return n.post(ctx, m)
}

// SendFloor notifies the admin — problems on the restaurant floor: an order
// nobody accepted, nothing printed, a rerouted ticket, unmatched items, a
// printer down / back up. Since 2.5.2 the store owner and the boss don't get
// these (they were a stream): they get SendOwner's few plain-English alerts.
func (n *Notifier) SendFloor(ctx context.Context, message, sound string) error {
	var errs []error
	for _, m := range n.floorMessages(message, sound) {
		if err := n.post(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) floorMessages(message, sound string) []Message {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.adminDevices) == 0 && n.strictScope {
		return nil // never the whole account outside prod (2.4.1)
	}
	// No admin devices (prod only): unscoped, the whole account.
	return []Message{{User: n.userKey, Devices: dedupe(n.adminDevices), Text: message, Sound: sound}}
}

// SendOwner notifies the store owner (2.5.2): the owner's devices on the
// account (location.config.pushover_owner_devices) and the boss key
// (PUSHOVER_BOSS_KEY, its own account). Never the whole account: with no
// owner devices and no separate boss key nothing is sent. Outside prod it
// sends only once admin devices are known (the same guard as SendFloor).
// application/owneralert decides what reaches it.
func (n *Notifier) SendOwner(ctx context.Context, title, message, sound string) error {
	var errs []error
	for _, m := range n.ownerMessages(title, message, sound) {
		if err := n.post(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OwnerConfigured reports whether SendOwner would page anyone.
func (n *Notifier) OwnerConfigured() bool {
	n.mu.RLock()
	ok := n.appToken != ""
	n.mu.RUnlock()
	return ok && len(n.ownerMessages("", "", "")) > 0
}

func (n *Notifier) ownerMessages(title, message, sound string) []Message {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.strictScope && len(n.adminDevices) == 0 {
		return nil
	}
	var msgs []Message
	if len(n.ownerDevices) > 0 && n.userKey != "" {
		msgs = append(msgs, Message{User: n.userKey, Devices: dedupe(n.ownerDevices), Title: title, Text: message, Sound: sound})
	}
	if n.bossKey != "" && n.bossKey != n.userKey {
		msgs = append(msgs, Message{User: n.bossKey, Title: title, Text: message, Sound: sound})
	}
	return msgs
}

// RouteSound: a route plays the long "persistent" sound once (2.3.10,
// owner: the notification itself is enough — 2.3.9 repeated it every 30 s
// until Acknowledge).
const RouteSound = "persistent"

// SendRoute sends a driver their route: high priority (skips the phone's
// quiet hours), the long sound once; the link opens the map. Unlike Send it reports why it
// couldn't: ErrNotConfigured, an *APIError, or a network error.
func (n *Notifier) SendRoute(ctx context.Context, device, title, text, link string) error {
	n.mu.RLock()
	m := Message{User: n.userKey, Devices: []string{device}, Title: title, Text: text,
		Priority: 1, Sound: RouteSound, URL: link, URLTitle: "Open route"}
	n.mu.RUnlock()
	if m.User == "" {
		return ErrNotConfigured
	}
	return n.post(ctx, m)
}

// SendDriver sends one driver a plain message on his own device (2.4.1:
// "come back, run on hold"): high priority, the phone's own sound. Like
// SendRoute it reports why it couldn't.
func (n *Notifier) SendDriver(ctx context.Context, device, title, text string) error {
	n.mu.RLock()
	m := Message{User: n.userKey, Devices: []string{device}, Title: title, Text: text, Priority: 1}
	n.mu.RUnlock()
	if m.User == "" {
		return ErrNotConfigured
	}
	return n.post(ctx, m)
}

// Message is one Pushover message.
type Message struct {
	User     string
	Devices  []string // empty = every device on the account
	Title    string
	Text     string
	Sound    string // empty = the phone's own sound
	Priority int
	// Retry / Expire: an emergency (priority 2) alert repeats every Retry
	// (>= 30 s) until acknowledged or Expire (<= 3 h) has passed.
	Retry    time.Duration
	Expire   time.Duration
	URL      string
	URLTitle string
}

func dedupe(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range in {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func (n *Notifier) post(ctx context.Context, m Message) error {
	n.mu.RLock()
	token := n.appToken
	n.mu.RUnlock()
	if token == "" || m.User == "" {
		if m.Priority > 0 { // a route: the caller tells the tablet
			return ErrNotConfigured
		}
		return nil // not configured (yet): an unlinked agent has no keys
	}
	form := url.Values{
		"token":   {token},
		"user":    {m.User},
		"message": {m.Text},
	}
	if m.Sound != "" {
		form.Set("sound", m.Sound)
	}
	if len(m.Devices) > 0 {
		form.Set("device", strings.Join(m.Devices, ","))
	}
	if m.Title != "" {
		form.Set("title", m.Title)
	}
	if m.Priority != 0 {
		form.Set("priority", fmt.Sprint(m.Priority))
	}
	if m.Priority == 2 {
		form.Set("retry", fmt.Sprint(int(m.Retry.Seconds())))
		form.Set("expire", fmt.Sprint(int(m.Expire.Seconds())))
	}
	if m.URL != "" {
		form.Set("url", m.URL)
		form.Set("url_title", m.URLTitle)
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
		if jsonErr := json.Unmarshal(body, &errData); jsonErr == nil && len(errData.Errors) > 0 {
			return &APIError{Status: resp.StatusCode, Errors: errData.Errors}
		}
		return fmt.Errorf("pushover returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
