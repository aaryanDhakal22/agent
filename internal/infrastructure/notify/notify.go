package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type Notifier struct {
	appToken    string
	userKey     string
	pushoverURL string
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
	}
}

func (n *Notifier) Send(message string) error {
	resp, err := http.PostForm(n.pushoverURL, url.Values{
		"token":   {n.appToken},
		"user":    {n.userKey},
		"message": {message},
	})
	if err != nil {
		var errData NotifierError
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&errData); err != nil {
			return fmt.Errorf("failed to decode pushover error response: %s", err)
		}
		return fmt.Errorf("Pushover error: %+v\n", errData)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println(resp.StatusCode)
		return fmt.Errorf("pushover returned status %d", resp.StatusCode)
	}

	return nil
}
