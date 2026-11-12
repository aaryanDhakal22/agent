package mainclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// R-5: the APK is streamed into the writer, capped at the release's size.
func TestDownloadToStreamsWithALimit(t *testing.T) {
	body := strings.Repeat("A", 100_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(storeAPIKeyHeader) != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL)
	var buf bytes.Buffer
	status, n, err := c.DownloadTo(context.Background(), "k", "/apk", "", &buf, int64(len(body)))
	if err != nil || status != 200 || n != int64(len(body)) || buf.String() != body {
		t.Fatalf("status %d n %d err %v", status, n, err)
	}
	buf.Reset()
	if _, _, err := c.DownloadTo(context.Background(), "k", "/apk", "", &buf, 10); err == nil {
		t.Fatal("a body over the limit must fail")
	}
	buf.Reset()
	if status, n, err := c.DownloadTo(context.Background(), "bad", "/apk", "", &buf, 10); status != 401 || n != 0 || err != nil || buf.Len() != 0 {
		t.Fatalf("401: %d %d %v", status, n, err)
	}
}
