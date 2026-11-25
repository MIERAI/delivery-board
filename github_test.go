package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// logLines captures what the board logs (JSON lines) while a test runs.
func logLines(t *testing.T) func(substr string) int {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(substr string) int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), substr)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func testClient(f *fakeServer, clock *time.Time) *gitHub {
	g := newGitHub("")
	g.api = f.URL + "/"
	g.now = func() time.Time { return *clock }
	return g
}

// Last-Modified alone (no ETag) makes the next request conditional too.
func TestLastModified(t *testing.T) {
	f := newFakeServer(map[string]any{"repos/o/a/commits?path=x": []any{map[string]any{"sha": "s1"}}})
	f.noETag, f.lastMod = true, true
	defer f.Close()
	clock := t0
	g := testClient(f, &clock)
	for i := range 3 {
		var v []ghCommit
		if err := g.Get("repos/o/a/commits?path=x", false, &v); err != nil || len(v) != 1 || v[0].SHA != "s1" {
			t.Fatalf("get %d: %v %v", i, v, err)
		}
	}
	if st, _, _ := f.take(); st[200] != 1 || st[304] != 2 {
		t.Fatalf("responses %v, want one 200 then 304s", st)
	}
	// changed: a 200 again, and the new body
	f.set("repos/o/a/commits?path=x", []any{map[string]any{"sha": "s2"}, map[string]any{"sha": "s1"}})
	var v []ghCommit
	if err := g.Get("repos/o/a/commits?path=x", false, &v); err != nil || len(v) != 2 {
		t.Fatalf("after a change: %v %v", v, err)
	}
}

// Pipelines collected in parallel that read the same path in one cycle: one request. The next cycle asks again.
func TestCycleDeduplicates(t *testing.T) {
	f := newFakeServer(map[string]any{"repos/o/m/pulls?state=open&per_page=30": []any{}})
	defer f.Close()
	clock := t0
	g := testClient(f, &clock)
	for range 2 {
		c := g.cycle()
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var v []ghPull
				if err := c.Get("repos/o/m/pulls?state=open&per_page=30", false, &v); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	if st, total, _ := f.take(); total != 2 || st[200] != 1 || st[304] != 1 {
		t.Fatalf("%d requests %v, want one per cycle (200, then 304)", total, st)
	}
	if n := g.metrics.deduplicated.Load(); n != 38 {
		t.Errorf("deduplicated %d, want 38", n)
	}
}

// Below the reserve (20% of the limit by default) refreshes pause until the limit resets, logged once; a promotion's
// refresh still runs.
func TestLowRemainingPauses(t *testing.T) {
	logged := logLines(t)
	f := newFakeServer(fleetData())
	defer f.Close()
	clock := t0
	b := fleetBoard(t, f, &clock)
	f.used = 4000 // 1000 left: the first cycle crosses the floor of 1000
	b.refresh()
	f.take()
	if b.GitHubNote() == "" {
		t.Fatalf("no note at %d of 5000 left", 5000-f.used)
	}
	for range 3 {
		clock = clock.Add(30 * time.Second)
		b.refresh()
	}
	if _, total, _ := f.take(); total != 0 {
		t.Fatalf("%d requests while below the reserve", total)
	}
	if n := b.gh.metrics.skipped.Load(); n != 3 {
		t.Errorf("skipped %d cycles, want 3", n)
	}
	if n := logged("refreshes paused"); n != 1 {
		t.Errorf("pause logged %d times, want once", n)
	}
	metrics := b.metrics()
	for _, want := range []string{"delivery_github_cycles_skipped_total 3", "delivery_github_ratelimit_limit 5000", "delivery_github_requests_total{result=\"200\"} 106"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if !strings.Contains(metrics, "delivery_github_ratelimit_remaining ") {
		t.Error("no remaining in the metrics")
	}
	// a promotion is not a background refresh: it reads (conditionally) even below the reserve
	if b.refreshOne(b.gh, b.pipeline("hello")) == nil {
		t.Fatal("refresh for a promotion failed")
	}
	if st, _, _ := f.take(); st[304] == 0 {
		t.Errorf("promotion refresh: %v", st)
	}
	// the limit resets: refreshes again
	f.used, f.reset = 0, f.reset.Add(time.Hour)
	clock = t0.Add(time.Hour + time.Second)
	b.refresh()
	if st, _, _ := f.take(); st[304] == 0 || b.GitHubNote() != "" {
		t.Fatalf("after the reset: %v, note %q", st, b.GitHubNote())
	}
	if n := logged("refreshing again"); n != 1 {
		t.Errorf("resume logged %d times", n)
	}
}

// reserve: 0 uses the whole limit.
func TestReserveZero(t *testing.T) {
	cfg, err := ParseConfig([]byte(testConfig + "github: { reserve: 0 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.GitHub.reserve(); r != 0 {
		t.Fatalf("reserve %g", r)
	}
	if _, err := ParseConfig([]byte(testConfig + "github: { reserve: 1.5 }\n")); err == nil || !strings.Contains(err.Error(), "github.reserve") {
		t.Fatalf("reserve 1.5: %v", err)
	}
	clock := t0
	g := newGitHub("")
	g.now = func() time.Time { return clock }
	g.reserve = 0
	g.rate = rateLimit{limit: 5000, remaining: 3, reset: t0.Add(time.Hour)}
	if !g.proceed() {
		t.Fatal("paused with 3 left and no reserve")
	}
}

// A 403 rate limit answer: no request until the reset GitHub names, refreshes skipped, logged once; then back.
func TestRateLimit403(t *testing.T) {
	logged := logLines(t)
	f := newFakeServer(fleetData())
	defer f.Close()
	clock := t0
	b := fleetBoard(t, f, &clock)
	b.refresh()
	f.take()
	reset := t0.Add(10 * time.Minute)
	f.override = func(w http.ResponseWriter, r *http.Request) int {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded for user ID 1."}`, http.StatusForbidden)
		return http.StatusForbidden
	}
	clock = clock.Add(30 * time.Second)
	b.refresh()
	_, first, _ := f.take()
	// pipelines collect in parallel: each may have one request in flight when the first 403 arrives, no more
	if first == 0 || first > len(b.Pipelines())+1 {
		t.Fatalf("%d requests in the cycle that hit the limit", first)
	}
	for _, s := range b.All() {
		if !strings.Contains(s.Error, "rate limit") || len(s.Runs) == 0 { // the last good data stays, with the error
			t.Fatalf("%s: error %q, %d runs", s.P.ID, s.Error, len(s.Runs))
		}
	}
	for range 5 {
		clock = clock.Add(time.Minute)
		b.refresh()
	}
	if _, total, _ := f.take(); total != 0 {
		t.Fatalf("%d requests before the reset", total)
	}
	if n := logged("GitHub rate limit"); n != 1 {
		t.Errorf("rate limit logged %d times, want once", n)
	}
	if n := logged(`"msg":"collect"`); n != 0 {
		t.Errorf("collect errors logged %d times for the rate limit", n)
	}
	if !strings.Contains(b.GitHubNote(), "rate limit") {
		t.Errorf("note %q", b.GitHubNote())
	}
	f.override, f.reset = nil, reset.Add(time.Hour)
	clock = reset.Add(time.Second)
	b.refresh()
	if st, _, _ := f.take(); st[304] == 0 || st[403] != 0 {
		t.Fatalf("after the reset: %v", st)
	}
	for _, s := range b.All() {
		if s.Error != "" {
			t.Fatalf("%s: %s", s.P.ID, s.Error)
		}
	}
}

// Secondary limits: 429 with Retry-After waits that long; a 403 that is not a rate limit (no permission) is an error.
func TestSecondaryLimitAndForbidden(t *testing.T) {
	f := newFakeServer(map[string]any{})
	defer f.Close()
	clock := t0
	g := testClient(f, &clock)
	f.override = func(w http.ResponseWriter, r *http.Request) int {
		w.Header().Set("Retry-After", "45")
		http.Error(w, `{"message":"You have exceeded a secondary rate limit."}`, http.StatusTooManyRequests)
		return http.StatusTooManyRequests
	}
	var v any
	err := g.Get("repos/o/a/pulls", false, &v)
	if rl, ok := err.(*RateLimitError); !ok || !rl.Until.Equal(t0.Add(45*time.Second)) {
		t.Fatalf("429: %v", err)
	}
	clock = t0.Add(44 * time.Second)
	if g.proceed() {
		t.Fatal("proceeds during Retry-After")
	}
	clock = t0.Add(46 * time.Second)
	if !g.proceed() {
		t.Fatal("still paused after Retry-After")
	}
	f.override = func(w http.ResponseWriter, r *http.Request) int {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
		return http.StatusForbidden
	}
	if err := g.Get("repos/o/a/pulls", false, &v); err == nil || isRateLimit(err) {
		t.Fatalf("403 without a rate limit: %v", err)
	}
	if !g.proceed() {
		t.Fatal("a permission error paused the board")
	}
}

func TestValidatorCacheBounded(t *testing.T) {
	c := newValidatorCache(3, 10)
	for i, p := range []string{"a", "b", "c", "d"} {
		c.put(p, &validated{etag: strconv.Itoa(i), body: []byte("12")})
	}
	if c.get("a") != nil || c.get("d") == nil || c.list.Len() != 3 {
		t.Fatalf("entries: a %v, %d kept", c.get("a"), c.list.Len())
	}
	c.get("b")                                       // b used: c is now the oldest
	c.put("e", &validated{body: []byte("12345678")}) // 2+2+2+8 > 10 bytes: c and d go
	if c.get("c") != nil || c.get("d") != nil || c.get("b") == nil || c.size != 10 {
		t.Fatalf("bytes: size %d, c %v d %v b %v", c.size, c.get("c"), c.get("d"), c.get("b"))
	}
	c.put("big", &validated{body: make([]byte, 11)}) // larger than the whole cache: not kept
	if c.get("big") != nil || c.size != 10 {
		t.Fatalf("big kept, size %d", c.size)
	}
	c.put("b", &validated{body: []byte("1")}) // replaced: its size counted once
	if c.size != 9 || c.list.Len() != 2 {
		t.Fatalf("replace: size %d, %d entries", c.size, c.list.Len())
	}
}
