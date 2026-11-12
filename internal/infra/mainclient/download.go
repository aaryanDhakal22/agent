package mainclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DownloadTo streams a large body (a tablet-app APK) into w instead of
// holding it in memory (hardening R-5): at most limit bytes are copied (more
// is an error). A non-200 answer copies nothing and returns its status. Same
// auth and timeout as Download.
func (c *Client) DownloadTo(ctx context.Context, key, path, rawQuery string, w io.Writer, limit int64) (int, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	url := c.baseURL + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set(storeAPIKeyHeader, key)
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, 0, nil
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, n, fmt.Errorf("read body: %w", err)
	}
	if n > limit {
		return resp.StatusCode, n, fmt.Errorf("body larger than %d bytes", limit)
	}
	return resp.StatusCode, n, nil
}
