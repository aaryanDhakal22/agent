package transport

import (
	"bytes"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// What a terminal may print through the Pi (POST /api/terminal/print, the
// security round C1b). The terminal renders its receipts with a small, fixed
// command set (terminal lib/platform/printer/escpos.dart `Cmd`); anything
// else in a job is refused, so a terminal's key can't open a drawer, change
// a printer's set-up or write its memory through the Pi.

const (
	esc = 0x1B
	gs  = 0x1D
)

// checkTerminalJob answers "" when data is made of the terminal renderer's
// commands only, else what is wrong (plain words, for the log and the 400).
//
// Allowed: printable bytes (>= 0x20: PC437 text), LF, CR, HT, and
//
//	ESC @            initialise
//	ESC t n          code page
//	ESC a n          align
//	ESC E n          bold
//	ESC d n          feed n lines
//	ESC M n          font
//	ESC - n          underline
//	GS ! n           size
//	GS B n           reverse
//	GS V m [n]       cut
//	GS v 0 m xL xH yL yH d...   a raster picture (the logo, struck lines)
func checkTerminalJob(data []byte) string {
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case b >= 0x20, b == '\n', b == '\r', b == '\t':
			i++
		case b == esc:
			if i+1 >= len(data) {
				return "a command cut short"
			}
			switch c := data[i+1]; c {
			case '@':
				i += 2
			case 't', 'a', 'E', 'd', 'M', '-':
				if i+2 >= len(data) {
					return "a command cut short"
				}
				i += 3
			case 'p':
				return "a drawer kick"
			default:
				return fmt.Sprintf("a command receipts don't use (ESC 0x%02X)", c)
			}
		case b == gs:
			if i+1 >= len(data) {
				return "a command cut short"
			}
			switch c := data[i+1]; c {
			case '!', 'B':
				if i+2 >= len(data) {
					return "a command cut short"
				}
				i += 3
			case 'V':
				if i+2 >= len(data) {
					return "a command cut short"
				}
				switch m := data[i+2]; m {
				case 0, 1, '0', '1':
					i += 3
				case 'A', 'B':
					if i+3 >= len(data) {
						return "a command cut short"
					}
					i += 4
				default:
					return "a cut receipts don't use"
				}
			case 'v':
				// GS v 0 m xL xH yL yH + xBytes * y data bytes.
				if i+7 >= len(data) || data[i+2] != '0' {
					return "a picture cut short"
				}
				n := (int(data[i+4]) | int(data[i+5])<<8) * (int(data[i+6]) | int(data[i+7])<<8)
				if n <= 0 || i+8+n > len(data) {
					return "a picture cut short"
				}
				i += 8 + n
			default:
				return fmt.Sprintf("a command receipts don't use (GS 0x%02X)", c)
			}
		case b == 0x10:
			return "a status or drawer command"
		default:
			return fmt.Sprintf("a control byte receipts don't use (0x%02X)", b)
		}
	}
	return ""
}

// cleanLabel keeps a test slip's label printable: ASCII letters, digits,
// spaces and plain punctuation, at most maxTestLabel characters.
func cleanLabel(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= maxTestLabel {
			break
		}
		if r > unicode.MaxASCII || r < 0x20 || r == 0x7F {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// terminalTestSlip is the slip the Pi prints for a terminal's test: built
// here, never from the terminal's bytes.
func terminalTestSlip(label, printer, terminalName string, now time.Time) []byte {
	var b bytes.Buffer
	b.Write([]byte{esc, '@'})
	b.Write([]byte{esc, 'a', 1}) // centre
	b.Write([]byte{esc, 'E', 1, gs, '!', 0x11})
	b.WriteString("TEST SLIP\n")
	b.Write([]byte{gs, '!', 0x00, esc, 'E', 0})
	b.WriteString("\n")
	if label != "" {
		b.WriteString(label + "\n")
	}
	b.WriteString("Printer: " + cleanLabel(printer) + "\n")
	if terminalName != "" {
		b.WriteString("From: " + cleanLabel(terminalName) + "\n")
	}
	b.WriteString(now.Format("2006-01-02 15:04:05") + "\n")
	b.WriteString("\nIf you can read this, a terminal can\nprint here through the Pi.\n")
	b.Write([]byte{esc, 'd', 4})
	b.Write([]byte{gs, 'V', 'B', 3})
	return b.Bytes()
}
