package web

import (
	"regexp"
	"testing"
)

// An element that starts hidden must actually be hidden. The UI hides with
// Alpine's x-show / x-cloak: [x-cloak] must be display:none !important, or an
// #id rule that sets display wins and the element shows before Alpine starts
// (the "YOUR RUN" driver flash once covered the whole page, setup and login
// included). If a .hidden class is ever used again it must be !important too.
func TestHiddenAlwaysWins(t *testing.T) {
	b, err := FS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if !regexp.MustCompile(`\[x-cloak\]\s*\{\s*display:\s*none\s*!important`).MatchString(css) {
		t.Error("app.css needs [x-cloak]{display:none !important}")
	}
	page, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	usesHidden := regexp.MustCompile(`class="[^"]*\bhidden\b`).Match(page)
	if usesHidden && !regexp.MustCompile(`\.hidden\s*\{\s*display:\s*none\s*!important`).MatchString(css) {
		t.Error("index.html uses class=hidden: .hidden must be display:none !important")
	}
	// An overlay that starts hidden via x-show must also carry x-cloak, or it
	// flashes before Alpine has run.
	for _, m := range regexp.MustCompile(`<[^>]*\bid="(flash|dialog|toasts)"[^>]*>`).FindAll(page, -1) {
		if !regexp.MustCompile(`\bx-cloak\b`).Match(m) {
			t.Errorf("overlay must start x-cloak: %s", m)
		}
	}
}
