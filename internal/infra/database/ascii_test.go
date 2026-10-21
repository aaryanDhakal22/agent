package database

import (
	"os"
	"strings"
	"testing"
)

// TestQuerySQLIsASCII guards sql/query.sql: a single non-ASCII byte (e.g. an
// arrow in a comment) makes sqlc's sqlite engine mis-slice every later query,
// which only shows up at runtime as a "syntax error".
func TestQuerySQLIsASCII(t *testing.T) {
	b, err := os.ReadFile("../../../sql/query.sql")
	if err != nil {
		t.Fatal(err)
	}
	line := 1
	for i, c := range b {
		if c == '\n' {
			line++
		}
		if c > 0x7F {
			start := strings.LastIndexByte(string(b[:i]), '\n') + 1
			end := i + strings.IndexByte(string(b[i:])+"\n", '\n')
			t.Fatalf("sql/query.sql line %d has a non-ASCII byte 0x%X: %q", line, c, b[start:end])
		}
	}
}
