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

func (n *Notifier) Send(message string, sound string) error {
	resp, _ := http.PostForm(n.pushoverURL, url.Values{
		"token":   {n.appToken},
		"user":    {n.userKey},
		"message": {message},
		"sound":   {sound},
	})
	if resp.StatusCode != http.StatusOK {
		var errData NotifierError
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&errData); err != nil {
			return fmt.Errorf("failed to decode pushover error response: %s", err)
		}
		return fmt.Errorf("Pushover error: %+v\n", errData)
	}
	defer resp.Body.Close()

	return nil
}
