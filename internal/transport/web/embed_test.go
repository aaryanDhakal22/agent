package web

import (
	"regexp"
	"strings"
	"testing"
)

// Every element that starts with class="hidden" must actually be hidden: an
// #id rule that sets display would otherwise win over .hidden (the "YOUR RUN"
// driver flash once covered the whole page, setup and login included).
func TestHiddenAlwaysWins(t *testing.T) {
	b, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	css := page[:strings.Index(page, "</style>")]
	if regexp.MustCompile(`\.hidden\s*\{\s*display:\s*none\s*!important`).MatchString(css) {
		return
	}
	idDisplay := map[string]bool{}
	for _, m := range regexp.MustCompile(`#([A-Za-z][\w-]*)\s*\{[^}]*display\s*:`).FindAllStringSubmatch(css, -1) {
		idDisplay[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`id="([\w-]+)"[^>]*class="[^"]*\bhidden\b`).FindAllStringSubmatch(page, -1) {
		if idDisplay[m[1]] {
			t.Errorf("#%s sets display in CSS and starts hidden: .hidden must be !important", m[1])
		}
	}
}
