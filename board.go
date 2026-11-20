package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

// Board keeps the latest collected state of every pipeline and serves it.
type Board struct {
	cfg          *Config
	gh           *gitHub
	kube         Kube
	promoteToken string // optional: Actions read/write on the promotion workflows' repositories
	now          func() time.Time

	mu       sync.RWMutex
	states   map[string]*PipelineState
	promoted map[string]time.Time // pipeline/env → last dispatch (rate limit)

	events doraEvents // DORA counters (under mu)

	discovered   []*Pipeline // from app.yaml descriptors (cfg.Discover)
	discoveredAt time.Time
	discoverErr  string

	cssVersion string

	liveMu sync.Mutex
	live   map[string]liveResult
}

func NewBoard(cfg *Config) *Board {
	return &Board{cfg: cfg, gh: newGitHub(os.Getenv("GITHUB_TOKEN")), kube: newKube(),
		promoteToken: os.Getenv("PROMOTE_TOKEN"), now: time.Now,
		states: map[string]*PipelineState{}, promoted: map[string]time.Time{}, live: map[string]liveResult{}}
}

func (b *Board) Loop() {
	for {
		b.refresh()
		time.Sleep(b.cfg.Interval.Duration)
	}
}

func (b *Board) refresh() {
	b.rediscover()
	pipes := b.Pipelines()
	b.mu.Lock() // forget pipelines that are gone (an app.yaml removed)
	for id := range b.states {
		if !slices.ContainsFunc(pipes, func(p *Pipeline) bool { return p.ID == id }) {
			delete(b.states, id)
		}
	}
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range pipes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.refreshOne(p)
		}()
	}
	wg.Wait()
}

func (b *Board) refreshOne(p *Pipeline) *PipelineState {
	ps, err := collectPipeline(b.gh, b.kube, p, b.cfg.MaxRuns, b.now)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		slog.Error("collect", "pipeline", p.ID, "err", err)
		prev := b.states[p.ID]
		if prev == nil {
			prev = &PipelineState{P: p}
		}
		cp := *prev // keep the last good data, show the error
		cp.Error = trunc(err.Error(), 240)
		b.states[p.ID] = &cp
		return nil
	}
	b.states[p.ID] = ps
	b.events.observe(ps)
	return ps
}

func (b *Board) State(id string) *PipelineState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.states[id]
}

func (b *Board) All() []*PipelineState {
	pipes := b.Pipelines()
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []*PipelineState
	for _, p := range pipes {
		if s := b.states[p.ID]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

func (b *Board) pipeline(id string) *Pipeline {
	for _, p := range b.Pipelines() {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Promote dispatches the promotion workflow of env with the candidate version, after re-checking the gate on fresh
// data. A person still merges the PR it opens.
func (b *Board) Promote(pipeline, env string) (string, error) {
	p := b.pipeline(pipeline)
	if p == nil || p.env(env) == nil || p.env(env).Promotion == nil {
		return "", fmt.Errorf("没有这个 promote: %s/%s", pipeline, env)
	}
	if b.promoteToken == "" {
		return "", fmt.Errorf("看板没有配置 PROMOTE_TOKEN: 请在 GitHub 上手动运行 workflow")
	}
	key := pipeline + "/" + env
	b.mu.Lock()
	if t, ok := b.promoted[key]; ok && b.now().Sub(t) < time.Minute {
		b.mu.Unlock()
		return "", fmt.Errorf("一分钟内刚发起过,请稍等")
	}
	b.mu.Unlock()
	ps := b.refreshOne(p)
	if ps == nil {
		return "", fmt.Errorf("刷新失败,没有发起")
	}
	g := ps.Gate(env)
	if !g.Ready {
		return "", fmt.Errorf("门槛没通过: %s", g.failing())
	}
	pr := p.env(env).Promotion
	inputs := map[string]string{}
	for k, v := range pr.Workflow.Inputs {
		inputs[k] = v
	}
	inputs[pr.Workflow.VersionInput] = g.Candidate
	if pr.Workflow.SoakInput != "" {
		inputs[pr.Workflow.SoakInput] = fmt.Sprint(int(pr.Soak.Minutes()))
	}
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/dispatches", pr.Workflow.Repo, pr.Workflow.File)
	if _, err := b.gh.do("POST", path, b.promoteToken, map[string]any{"ref": pr.Workflow.Ref, "inputs": inputs}); err != nil {
		return "", err
	}
	b.mu.Lock()
	b.promoted[key] = b.now()
	b.mu.Unlock()
	slog.Info("promote dispatched", "pipeline", pipeline, "env", env, "version", g.Candidate)
	return g.Candidate, nil
}

// ── try it ─────────────────────────────────────────────────────────────────────────────────────────────────────────

type CatalogEntry struct {
	Method  string `json:"method"`
	Summary string `json:"summary"`
	Path    string `json:"path"`
	Cases   []struct {
		Name     string          `json:"name"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	} `json:"cases"`
}

func (b *Board) catalog(ps *PipelineState) map[string]*CatalogEntry {
	t := ps.P.Try
	env := ps.Env(t.Env)
	if env == nil || env.Tag == "" {
		return nil
	}
	var f struct {
		Content string `json:"content"`
	}
	if err := b.gh.Get(fmt.Sprintf("repos/%s/contents/%s?ref=%s", ps.P.Repo, t.Catalog, env.Tag), true, &f); err != nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(stripNL(f.Content))
	if err != nil {
		return nil
	}
	var list []*CatalogEntry
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := map[string]*CatalogEntry{}
	for _, a := range list {
		out[a.Method] = a
	}
	return out
}

type liveResult struct {
	at     time.Time
	Code   int
	Body   string
	Millis int64
}

// Live calls the item on the try environment (cached for a minute).
func (b *Board) Live(t *Try, path string, body []byte) liveResult {
	key := path + "\x00" + string(body)
	b.liveMu.Lock()
	if r, ok := b.live[key]; ok && b.now().Sub(r.at) < time.Minute {
		b.liveMu.Unlock()
		return r
	}
	b.liveMu.Unlock()
	req, _ := http.NewRequest("POST", t.InternalURL+path, bytesReader(body))
	req.Header.Set("Content-Type", "application/json")
	if t.TokenEnv != "" {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(t.TokenEnv))
	}
	t0 := time.Now()
	res := liveResult{at: b.now()}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		res.Body = trunc(err.Error(), 160)
	} else {
		defer resp.Body.Close()
		res.Code = resp.StatusCode
		res.Body = prettyJSON(readAll(resp.Body, 64<<10))
	}
	res.Millis = time.Since(t0).Milliseconds()
	b.liveMu.Lock()
	b.live[key] = res
	b.liveMu.Unlock()
	return res
}

// for the templates
func (b *Board) Title() string { return b.cfg.Title }
func (b *Board) DiscoverError() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.discoverErr
}
func (b *Board) CSSVersion() string { return b.cssVersion }
func (b *Board) CanPromote() bool   { return b.promoteToken != "" }
