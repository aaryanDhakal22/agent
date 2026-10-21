package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// AgentFile is the agent binary's name in the signed manifest.
const AgentFile = "agent-linux-arm64"

// DefaultUtilityURL serves the signed per-env manifests (<url>/<env>/manifest.txt).
const DefaultUtilityURL = "https://utility.quiccpos.com"

const (
	latestTTL      = 10 * time.Minute
	latestRetryTTL = time.Minute // after a failed fetch
	latestTimeout  = 8 * time.Second
	maxManifest    = 64 << 10
)

// Manifest is the part of a signed release manifest the agent shows. Its
// signature is NOT checked here — this is display only; the root updater
// verifies the signature and every SHA-256 before it installs anything.
type Manifest struct {
	Header map[string]string // "# key=value" lines
	Files  map[string]string // file name -> sha256 (hex)
}

// AgentVersion is the manifest's agent_version header.
func (m Manifest) AgentVersion() string { return m.Header["agent_version"] }

// AgentSHA is the agent binary's sha256 in the manifest.
func (m Manifest) AgentSHA() string { return m.Files[AgentFile] }

// ParseManifest reads a manifest.txt: "# key=value" header lines and
// "<sha256>  <file name>" body lines (sha256sum output). It needs an
// agent_version and an agent-linux-arm64 line.
func ParseManifest(b []byte) (Manifest, error) {
	m := Manifest{Header: map[string]string{}, Files: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			kv := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if k, v, ok := strings.Cut(kv, "="); ok {
				m.Header[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || !isHexSHA(f[0]) {
			continue
		}
		m.Files[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if m.AgentVersion() == "" {
		return m, errors.New("manifest has no agent_version")
	}
	if m.AgentSHA() == "" {
		return m, fmt.Errorf("manifest has no %s line", AgentFile)
	}
	return m, nil
}

func isHexSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ManifestURL is where this env's manifest lives ("" = no updates for env).
func ManifestURL(utilityURL, env string) string {
	if env == "" {
		return ""
	}
	if utilityURL == "" {
		utilityURL = DefaultUtilityURL
	}
	return strings.TrimRight(utilityURL, "/") + "/" + env + "/manifest.txt"
}

// FileSHA256 hashes a file (the running binary: os.Executable()).
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Latest caches the newest manifest for this Pi's env (10 min; a failed
// fetch is retried after a minute) and knows the running binary's sha256,
// computed once. Get never blocks on the network: it starts a background
// refresh when the cache is stale and returns what it has.
type Latest struct {
	url    string
	client *http.Client
	ownSHA func() string

	mu         sync.Mutex
	fetchedAt  time.Time
	ttl        time.Duration
	version    string
	sha        string
	err        error
	refreshing bool
}

// NewLatest checks url ("" = never). exe is the running binary (its sha256
// is computed once, on first use).
func NewLatest(url, exe string) *Latest {
	var once sync.Once
	var sum string
	return &Latest{
		url:    url,
		client: &http.Client{Timeout: latestTimeout, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		ownSHA: func() string {
			once.Do(func() {
				if exe != "" {
					sum, _ = FileSHA256(exe)
				}
			})
			return sum
		},
	}
}

// OwnSHA is the running binary's sha256 ("" if unreadable).
func (l *Latest) OwnSHA() string {
	if l == nil || l.ownSHA == nil {
		return ""
	}
	return l.ownSHA()
}

// Get returns the cached manifest agent version + sha (refreshing in the
// background when stale). available = the manifest's binary differs from
// the running one (both known).
func (l *Latest) Get(ctx context.Context) (version string, available bool) {
	if l == nil || l.url == "" {
		return "", false
	}
	l.mu.Lock()
	stale := l.fetchedAt.IsZero() || time.Since(l.fetchedAt) > l.ttl
	if stale && !l.refreshing {
		l.refreshing = true
		go l.refresh(context.WithoutCancel(ctx))
	}
	version, sha := l.version, l.sha
	l.mu.Unlock()
	own := l.OwnSHA()
	return version, sha != "" && own != "" && !strings.EqualFold(sha, own)
}

// Refresh fetches now (tests, and the first check at startup).
func (l *Latest) Refresh(ctx context.Context) error {
	l.mu.Lock()
	l.refreshing = true
	l.mu.Unlock()
	return l.refresh(ctx)
}

func (l *Latest) refresh(ctx context.Context) error {
	m, err := l.fetch(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshing = false
	l.fetchedAt = time.Now()
	l.err = err
	if err != nil {
		l.ttl = latestRetryTTL
		return err
	}
	l.ttl = latestTTL
	l.version, l.sha = m.AgentVersion(), m.AgentSHA()
	return nil
}

func (l *Latest) fetch(ctx context.Context) (Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, latestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, nil)
	if err != nil {
		return Manifest{}, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return Manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("manifest: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest))
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(b)
}
