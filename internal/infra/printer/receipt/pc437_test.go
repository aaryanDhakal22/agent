package receipt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToPC437(t *testing.T) {
	if n := utf8.RuneCountInString(cp437High); n != 128 {
		t.Fatalf("cp437High has %d characters, want 128", n)
	}
	for in, want := range map[string]string{
		"Jalapeño":                 "Jalape\xa4o",
		"Crème Brûlée":             "Cr\x8ame Br\x96l\x82e",
		"FERNÁNDEZ RODRÍGUEZ":      "FERNANDEZ RODRIGUEZ",
		"Pizza 🍕 Special 🔥":        "Pizza ? Special ?",
		"👍🏽 ok":                    "? ok",
		"a​b️":                     "ab",
		"½ \xab \xfa":              "\xab \xab \xfa", // ½ and bytes already in PC437
		"plain ASCII \x1b\x21\x10": "plain ASCII \x1b\x21\x10",
		"\x1bJ\xc3":                "\x1bJ\xc3", // a command's argument byte stays
	} {
		if got := toPC437(in); got != want {
			t.Errorf("toPC437(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(in, "\x1bJ") {
			continue
		}
		if got := len(toPC437(in)); got > utf8.RuneCountInString(in) {
			t.Errorf("toPC437(%q) is %d bytes, more than its %d characters", in, got, utf8.RuneCountInString(in))
		}
	}
	// pictures pass untouched (their dots are any bytes)
	img := strikeImage("Ñandú")
	if toPC437(img) != img {
		t.Error("an ESC * picture was changed")
	}
	if got := foldASCII("Jalapeños Crème Brûlée ÁÑ 🍕"); got != "Jalapenos Creme Brulee AN 🍕" {
		t.Errorf("foldASCII = %q", got)
	}
}
