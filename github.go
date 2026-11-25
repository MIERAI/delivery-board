package main

import (
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GitHub reads the REST API. Every refresh reads the same lists again (commits, runs, pull requests, the history of
// the manifests files), and a user's tokens share one rate limit (5000 requests an hour), so the client spends as
// little of it as it can:
//
//   - Objects that cannot change any more (a finished run's jobs, a file at a commit) are kept for the life of the
//     process and never asked for again.
//   - Everything else is asked for conditionally: the last ETag / Last-Modified of each path is kept (with its body,
//     in a bounded LRU) and sent as If-None-Match / If-Modified-Since. A 304 reuses the body and does not count
//     against the rate limit.
//   - One refresh (cycle) asks for a path once, however many pipelines read it (the same repository, the same
//     manifests repository's open PRs).
//   - The rate limit headers are read on every response. Below the reserve (a share of the limit left to the other
//     clients of the account) refreshes pause until the limit resets; after a 403 / 429 rate limit answer no request is
//     sent until the time GitHub names (Retry-After, X-RateLimit-Reset).
type GitHub interface {
	Get(path string, final bool, v any) error
}

const (
	validatorEntries = 2000     // paths whose last answer is kept for conditional requests
	validatorBytes   = 64 << 20 // and the bodies kept with them, at most
	defaultReserve   = 0.2
)

type gitHub struct {
	api, token string
	client     *http.Client
	now        func() time.Time
	reserve    float64 // share of the rate limit left to other clients of the account

	mu      sync.Mutex
	final   map[string][]byte // objects that cannot change
	cache   *validatorCache   // the last answer of other paths, for conditional requests
	rate    rateLimit
	logged  time.Time // the end of the pause last logged (zero: not paused)
	metrics ghMetrics
}

type rateLimit struct {
	limit, remaining int       // X-RateLimit-Limit / -Remaining (limit 0: not seen yet)
	reset            time.Time // X-RateLimit-Reset
	blocked          time.Time // a rate limit answer (403 / 429): no request before
}

type ghMetrics struct {
	ok, notModified, failed, limited atomic.Int64 // requests by result
	deduplicated                     atomic.Int64 // GETs answered by another pipeline's request in the same cycle
	skipped                          atomic.Int64 // refresh cycles not run for the budget
}

func newGitHub(token string) *gitHub {
	return &gitHub{api: "https://api.github.com/", token: token, client: &http.Client{Timeout: 20 * time.Second},
		now: time.Now, reserve: defaultReserve, final: map[string][]byte{}, cache: newValidatorCache(validatorEntries, validatorBytes)}
}

// RateLimitError: GitHub answered with its rate limit (or did before): no request is sent before Until.
type RateLimitError struct{ Until time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit: no requests until " + e.Until.UTC().Format("15:04:05 UTC")
}

func (g *gitHub) Get(path string, final bool, v any) error {
	b, err := g.get(path, final)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (g *gitHub) get(path string, final bool) ([]byte, error) {
	g.mu.Lock()
	if b, ok := g.final[path]; ok {
		g.mu.Unlock()
		return b, nil
	}
	if now := g.now(); now.Before(g.rate.blocked) {
		until := g.rate.blocked
		g.mu.Unlock()
		return nil, &RateLimitError{until}
	}
	last := g.cache.get(path)
	g.mu.Unlock()
	hdr := http.Header{}
	if last != nil {
		if last.etag != "" {
			hdr.Set("If-None-Match", last.etag)
		}
		if last.modified != "" {
			hdr.Set("If-Modified-Since", last.modified)
		}
	}
	resp, body, err := g.send("GET", path, g.token, nil, hdr)
	if err != nil {
		g.metrics.failed.Add(1)
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && last != nil:
		g.metrics.notModified.Add(1)
		return last.body, nil
	case resp.StatusCode < 300:
		g.metrics.ok.Add(1)
		etag, modified := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		g.mu.Lock()
		if final {
			g.final[path] = body
		} else if etag != "" || modified != "" {
			g.cache.put(path, &validated{etag: etag, modified: modified, body: body})
		}
		g.mu.Unlock()
		return body, nil
	}
	if until, ok := rateLimited(resp, body, g.now()); ok {
		g.metrics.limited.Add(1)
		g.block(until)
		return nil, &RateLimitError{until}
	}
	g.metrics.failed.Add(1)
	return nil, statusError("GET", path, resp, body)
}

// do sends a request that is not cached (the promotion's workflow dispatch).
func (g *gitHub) do(method, path, token string, body any) ([]byte, error) {
	resp, b, err := g.send(method, path, token, body, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, statusError(method, path, resp, b)
	}
	return b, nil
}

func (g *gitHub) send(method, path, token string, body any, hdr http.Header) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, g.api+path, rd)
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "delivery-board")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if token == g.token { // the promotion token may belong to another account: its limit is not ours
		g.observe(resp.Header)
	}
	return resp, b, nil
}

func statusError(method, path string, resp *http.Response, body []byte) error {
	return fmt.Errorf("GitHub %s %s: %s %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, trunc(string(body), 160))
}

// observe records the rate limit headers. Responses of parallel requests arrive in any order: within one window
// (same reset) the lowest remaining count is the latest.
func (g *gitHub) observe(h http.Header) {
	limit, err1 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, err2 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err3 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || limit <= 0 {
		return
	}
	at := time.Unix(reset, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	if at.Equal(g.rate.reset) && remaining > g.rate.remaining {
		return
	}
	g.rate.limit, g.rate.remaining, g.rate.reset = limit, remaining, at
}

// rateLimited: a 403 / 429 that is GitHub's rate limit (primary or secondary), and until when to wait. Its advice:
// Retry-After when given, else the reset when nothing remains, else at least a minute.
func rateLimited(resp *http.Response, body []byte, now time.Time) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return now.Add(time.Duration(max(s, 1)) * time.Second), true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(reset, 0).After(now) {
			return time.Unix(reset, 0), true
		}
		return now.Add(time.Minute), true
	}
	if resp.StatusCode == http.StatusTooManyRequests || bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
		return now.Add(time.Minute), true
	}
	return time.Time{}, false // e.g. no permission
}

func (g *gitHub) block(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !until.After(g.rate.blocked) {
		return
	}
	g.rate.blocked = until
	if until.After(g.logged) {
		g.logged = until
		slog.Warn("GitHub rate limit: no requests until it resets", "until", until)
	}
}

// hold: whether refreshes should wait for the rate limit, until when and why ("" = go ahead).
func (g *gitHub) hold() (time.Time, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holdLocked(g.now())
}

func (g *gitHub) holdLocked(now time.Time) (time.Time, string) {
	if now.Before(g.rate.blocked) {
		return g.rate.blocked, "GitHub answered with its rate limit"
	}
	r := g.rate
	if floor := int(g.reserve * float64(r.limit)); r.limit > 0 && r.remaining < floor && now.Before(r.reset) {
		return r.reset, fmt.Sprintf("%d of %d requests left, below the %d kept for other clients", r.remaining, r.limit, floor)
	}
	return time.Time{}, ""
}

// proceed: may a refresh cycle run now? A pause is logged when it starts and when it ends, never per request.
func (g *gitHub) proceed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, why := g.holdLocked(g.now())
	if why != "" {
		g.metrics.skipped.Add(1)
		if !until.Equal(g.logged) {
			g.logged = until
			slog.Warn("GitHub API budget: refreshes paused", "until", until, "reason", why)
		}
		return false
	}
	if !g.logged.IsZero() {
		g.logged = time.Time{}
		slog.Info("GitHub API budget: refreshing again", "remaining", g.rate.remaining, "limit", g.rate.limit)
	}
	return true
}

// cycle: a view of the client for one refresh, asking for each path once whatever number of pipelines read it.
func (g *gitHub) cycle() GitHub {
	return &ghCycle{g: g, calls: map[string]*ghCall{}}
}

type ghCycle struct {
	g     *gitHub
	mu    sync.Mutex
	calls map[string]*ghCall
}

type ghCall struct {
	done chan struct{}
	body []byte
	err  error
}

func (c *ghCycle) Get(path string, final bool, v any) error {
	c.mu.Lock()
	call, ok := c.calls[path]
	if !ok {
		call = &ghCall{done: make(chan struct{})}
		c.calls[path] = call
		c.mu.Unlock()
		call.body, call.err = c.g.get(path, final)
		close(call.done)
	} else {
		c.mu.Unlock()
		<-call.done
		c.g.metrics.deduplicated.Add(1)
	}
	if call.err != nil {
		return call.err
	}
	return json.Unmarshal(call.body, v)
}

// writeMetrics: Prometheus text of the client's counters and the last rate limit seen.
func (g *gitHub) writeMetrics(w io.Writer) {
	m := &g.metrics
	fmt.Fprintf(w, "# HELP delivery_github_requests_total GitHub REST requests by result: 200 (counted against the rate limit), 304 (not modified: free), rate_limited (403/429 rate limit), error\n# TYPE delivery_github_requests_total counter\n")
	for _, r := range []struct {
		result string
		n      int64
	}{{"200", m.ok.Load()}, {"304", m.notModified.Load()}, {"rate_limited", m.limited.Load()}, {"error", m.failed.Load()}} {
		fmt.Fprintf(w, "delivery_github_requests_total{result=%q} %d\n", r.result, r.n)
	}
	fmt.Fprintf(w, "# HELP delivery_github_deduplicated_total GitHub GETs answered by another pipeline's request in the same refresh\n# TYPE delivery_github_deduplicated_total counter\ndelivery_github_deduplicated_total %d\n", m.deduplicated.Load())
	fmt.Fprintf(w, "# HELP delivery_github_cycles_skipped_total refreshes not run to keep the GitHub API budget (below the reserve, or rate limited)\n# TYPE delivery_github_cycles_skipped_total counter\ndelivery_github_cycles_skipped_total %d\n", m.skipped.Load())
	g.mu.Lock()
	r, entries, size := g.rate, g.cache.list.Len(), g.cache.size
	g.mu.Unlock()
	if r.limit > 0 {
		fmt.Fprintf(w, "# HELP delivery_github_ratelimit_remaining requests left in the current rate limit window (last response)\n# TYPE delivery_github_ratelimit_remaining gauge\ndelivery_github_ratelimit_remaining %d\n", r.remaining)
		fmt.Fprintf(w, "# HELP delivery_github_ratelimit_limit requests per rate limit window\n# TYPE delivery_github_ratelimit_limit gauge\ndelivery_github_ratelimit_limit %d\n", r.limit)
		fmt.Fprintf(w, "# HELP delivery_github_ratelimit_reset_timestamp_seconds when the rate limit window resets\n# TYPE delivery_github_ratelimit_reset_timestamp_seconds gauge\ndelivery_github_ratelimit_reset_timestamp_seconds %d\n", r.reset.Unix())
	}
	fmt.Fprintf(w, "# HELP delivery_github_cache_entries paths kept for conditional requests\n# TYPE delivery_github_cache_entries gauge\ndelivery_github_cache_entries %d\n", entries)
	fmt.Fprintf(w, "# HELP delivery_github_cache_bytes bodies kept for conditional requests\n# TYPE delivery_github_cache_bytes gauge\ndelivery_github_cache_bytes %d\n", size)
}

// ── validator cache ────────────────────────────────────────────────────────────────────────────────────────────────

// validated: the last answer of a path, with what makes the next request conditional.
type validated struct {
	path, etag, modified string
	body                 []byte
}

// validatorCache: least recently used first out, bounded by entries and by the bytes of the bodies. Not safe for
// concurrent use (gitHub.mu guards it).
type validatorCache struct {
	maxEntries, maxBytes, size int
	list                       *list.List // front: most recently used
	items                      map[string]*list.Element
}

func newValidatorCache(entries, bytes int) *validatorCache {
	return &validatorCache{maxEntries: entries, maxBytes: bytes, list: list.New(), items: map[string]*list.Element{}}
}

func (c *validatorCache) get(path string) *validated {
	if e, ok := c.items[path]; ok {
		c.list.MoveToFront(e)
		return e.Value.(*validated)
	}
	return nil
}

func (c *validatorCache) put(path string, v *validated) {
	v.path = path
	if e, ok := c.items[path]; ok {
		c.size -= len(e.Value.(*validated).body)
		c.list.Remove(e)
		delete(c.items, path)
	}
	if len(v.body) > c.maxBytes {
		return
	}
	c.items[path] = c.list.PushFront(v)
	c.size += len(v.body)
	for c.list.Len() > c.maxEntries || c.size > c.maxBytes {
		old := c.list.Back()
		ov := old.Value.(*validated)
		c.size -= len(ov.body)
		c.list.Remove(old)
		delete(c.items, ov.path)
	}
}

// isRateLimit: err is (or wraps) a rate limit answer, logged once by the client.
func isRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}
