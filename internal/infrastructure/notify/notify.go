package notify

import (
	"fmt"
	"net/http"
	"net/url"
)

type Notifier struct {
	appToken    string
	userKey     string
	pushoverURL string
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
		fmt.Println(err)
		return fmt.Errorf("pushover request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println(resp.StatusCode)
		return fmt.Errorf("pushover returned status %d", resp.StatusCode)
	}

	return nil
}
