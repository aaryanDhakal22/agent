package receipt

import (
	"strings"
	"unicode/utf8"
)

// The printers print code page 437 (what ESC @ selects), not UTF-8: a "ñ" sent as its two UTF-8 bytes
// prints "├▒", an emoji four characters of junk that also push the line past the paper (the print torture
// found both, quicc-ops tests/printcheck). toPC437 turns the text of a job into the printer's bytes, like
// the terminal's printable() (terminal/lib/platform/printer/escpos.dart): a character PC437 has becomes its
// byte, an accented capital it lacks its plain letter (Á → A), zero-width marks and emoji modifiers go, and
// anything else (an emoji) prints "?". One character in = at most one byte out, so wordWrap's rune counts
// still hold.
//
// It is the writer of every builder, so the builders' own commands pass through it too: it cannot drop the
// ASCII control bytes (cleanText, scrub.go, takes them out of every text before it gets here), only the
// ones that arrive as characters (C1, the Unicode line and paragraph separators).
//
// Only valid multi-byte UTF-8 is touched: ASCII, the commands and a byte already in PC437 ("\xab" = ½,
// "\xfa" = ·, which are invalid UTF-8 on their own) pass as they are. A picture (ESC * / GS v 0) is never
// passed through it: its dots are any bytes.

// cp437High is PC437 0x80-0xFF.
const cp437High = "ÇüéâäàåçêëèïîìÄÅÉæÆôöòûùÿÖÜ¢£¥₧ƒáíóúñÑªº¿⌐¬½¼¡«»░▒▓│┤╡╢╖╕╣║╗╝╜╛┐└┴┬├─┼╞╟╚╔╩╦╠═╬╧╨╤╥╙╘╒╓╫╪┘┌█▄▌▐▀αßΓπΣσµτΦΘΩδ∞φε∩≡±≥≤⌠⌡÷≈°∙·√ⁿ²■\u00a0"

var pc437 = func() map[rune]byte {
	m := map[rune]byte{}
	i := 0
	for _, r := range cp437High {
		m[r] = byte(0x80 + i)
		i++
	}
	return m
}()

// fold437: characters PC437 lacks, spelled with what it has.
var fold437 = map[rune]string{
	'Á': "A", 'À': "A", 'Â': "A", 'Ã': "A", 'ã': "a", 'È': "E", 'Ê': "E", 'Ë': "E", 'Í': "I", 'Ì': "I",
	'Î': "I", 'Ï': "I", 'Ó': "O", 'Ò': "O", 'Ô': "O", 'Õ': "O", 'õ': "o", 'Ú': "U", 'Ù': "U", 'Û': "U",
	'Ý': "Y", 'ý': "y", 'Ÿ': "Y", 'Ø': "O", 'ø': "o", 'Œ': "O", 'œ': "o", 'Š': "S", 'š': "s", 'Ž': "Z",
	'ž': "z", 'Č': "C", 'č': "c", 'Ł': "L", 'ł': "l", 'ő': "o", 'ű': "u",
	'‘': "'", '’': "'", '‚': ",", '“': "\"", '”': "\"", '„': "\"", '–': "-", '—': "-", '‐': "-", '‑': "-",
	'…': ".", '•': "\xf9", '×': "x", '\u2028': " ", '\u2029': " ",
}

// gone437: marks that print nothing (zero-width, joiners, variation selectors, emoji skin tones).
func gone437(r rune) bool {
	switch {
	case r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u2060' || r == '\ufeff':
		return true
	case r >= '\ufe00' && r <= '\ufe0f', r >= 0x1F3FB && r <= 0x1F3FF, r >= 0xE0020 && r <= 0xE007F:
		return true
	case r >= 0x0300 && r <= 0x036F: // combining accents: the letter before stays
		return true
	case r >= 0x80 && r <= 0x9F: // the C1 control characters: never a byte on the wire
		return true
	}
	return false
}

func toPC437(s string) string {
	if s == "" || strings.HasPrefix(s, "\x1b*") || strings.HasPrefix(s, "\x1dv0") {
		return s
	}
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1: // a raw byte (already PC437, or a command's argument)
			b.WriteByte(s[i])
		case r < 0x80:
			b.WriteByte(byte(r))
		case gone437(r):
		default:
			if c, ok := pc437[r]; ok {
				b.WriteByte(c)
			} else if f, ok := fold437[r]; ok {
				b.WriteString(f)
			} else {
				b.WriteByte('?')
			}
		}
		i += n
	}
	return b.String()
}

// plain437 is the plain letter of each accented letter PC437 has (the struck-through pictures draw from an
// ASCII font: a "ñ" drawn as "?" read "Jalape?os", the print torture found).
var plain437 = map[rune]rune{
	'Ç': 'C', 'ü': 'u', 'é': 'e', 'â': 'a', 'ä': 'a', 'à': 'a', 'å': 'a', 'ç': 'c', 'ê': 'e', 'ë': 'e', 'è': 'e',
	'ï': 'i', 'î': 'i', 'ì': 'i', 'Ä': 'A', 'Å': 'A', 'É': 'E', 'æ': 'a', 'Æ': 'A', 'ô': 'o', 'ö': 'o', 'ò': 'o',
	'û': 'u', 'ù': 'u', 'ÿ': 'y', 'Ö': 'O', 'Ü': 'U', 'á': 'a', 'í': 'i', 'ó': 'o', 'ú': 'u', 'ñ': 'n', 'Ñ': 'N',
}

// foldASCII spells text in ASCII for a picture drawn from the 6x13 font: accents dropped, characters it
// has no glyph for left to the picture ("?"), zero-width marks gone. One character in, at most one out.
func foldASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case gone437(r):
		default:
			if p, ok := plain437[r]; ok {
				b.WriteRune(p)
			} else if f, ok := fold437[r]; ok && len(f) == 1 && f[0] < 0x80 {
				b.WriteString(f)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
