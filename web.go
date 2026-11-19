package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

var words = map[string]string{Done: "成功", Running: "进行中", Person: "等人处理", Failed: "失败", Waiting: "未开始", Skip: "跳过"}

var svg = map[string]string{ // 16px icons, stroke = currentColor
	Done:    `<path d="M3.5 8.5l3 3 6-7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>`,
	Failed:  `<path d="M4.5 4.5l7 7m0-7l-7 7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Running: `<path d="M8 2a6 6 0 1 1-6 6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Person:  `<path d="M6 4v8M10 4v8" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Waiting: `<circle cx="8" cy="8" r="2" fill="currentColor"/>`,
	Skip:    `<path d="M4 8h8" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
}

func icon(st string, size int) template.HTML {
	spin := ""
	if st == Running {
		spin = ` class="spin"`
	}
	in := size * 64 / 100
	return template.HTML(fmt.Sprintf(`<span class="ico ico-%s" style="width:%dpx;height:%dpx"><svg viewBox="0 0 16 16" width="%d" height="%d"%s>%s</svg></span>`,
		st, size, size, in, in, spin, svg[st]))
}

func (b *Board) funcs() template.FuncMap {
	loc := b.cfg.loc
	return template.FuncMap{
		"icon":  icon,
		"word":  func(st string) string { return words[st] },
		"human": human,
		"dur": func(a, z time.Time) string {
			if a.IsZero() {
				return "—"
			}
			if z.IsZero() {
				z = b.now()
			}
			return human(z.Sub(a))
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			s := int(b.now().Sub(t).Seconds())
			switch {
			case s < 60:
				return "刚刚"
			case s < 3600:
				return fmt.Sprintf("%d 分钟前", s/60)
			case s < 86400:
				return fmt.Sprintf("%d 小时前", s/3600)
			}
			return t.In(loc).Format("01-02 15:04")
		},
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.In(loc).Format("01-02 15:04")
		},
		"hms": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc).Format("15:04:05")
		},
		"groups": groups,
		"link":   linkClass,
		"stepsub": func(s Step) string {
			var parts []string
			for _, x := range []string{s.Note, map[bool]string{true: human(dz(s.End, b.now()).Sub(s.Start))}[!s.Start.IsZero()], func() string {
				if t := first(s.End, s.Start); !t.IsZero() {
					return t.In(loc).Format("01-02 15:04")
				}
				return ""
			}()} {
				if x != "" {
					parts = append(parts, x)
				}
			}
			if len(parts) == 0 {
				return words[s.Status]
			}
			return strings.Join(parts, " · ")
		},
		"now": func(r *Run) string {
			if r.Status() == Done {
				return "已全部上线"
			}
			c := r.Current()
			if c == nil {
				return ""
			}
			return strings.TrimSuffix(c.Stage+" · "+c.Name+" · "+c.Note, " · ")
		},
		"lower": strings.ToLower,
		"spanOf": func(r *Run) []time.Time {
			a, z := r.Span()
			return []time.Time{a, z}
		},
	}
}

func dz(t, def time.Time) time.Time {
	if t.IsZero() {
		return def
	}
	return t
}

type group struct {
	Stage  string
	Status string
	Steps  []Step
}

func groups(steps []Step) []group {
	var out []group
	for _, s := range steps {
		if len(out) == 0 || out[len(out)-1].Stage != s.Stage {
			out = append(out, group{Stage: s.Stage})
		}
		out[len(out)-1].Steps = append(out[len(out)-1].Steps, s)
	}
	for i := range out {
		out[i].Status = overall(out[i].Steps)
	}
	return out
}

// linkClass: the connector between two stages (done: solid, running: flowing, waiting for a person: amber dashes).
func linkClass(prev, next string) string {
	fin := func(s string) bool { return s == Done || s == Skip }
	switch {
	case fin(prev) && fin(next):
		return "done"
	case next == Running:
		return "flow"
	case next == Person:
		return "wait"
	}
	return ""
}

func (b *Board) templates() *template.Template {
	return template.Must(template.New("").Funcs(b.funcs()).ParseFS(webFS, "web/*.html"))
}

type pageData struct {
	Title    string
	Refresh  int
	Board    *Board
	States   []*PipelineState
	Filter   string
	Flash    string
	Error    string
	Rows     []row    // list page: runs of every shown pipeline, newest first
	EnvNames []string // list page: the environment columns (every pipeline's, in order)
	// run page
	State *PipelineState
	Run   *Run
	Try   []tryItem
}

type row struct {
	S *PipelineState
	R *Run
}

type tryItem struct {
	Method, Summary, Path string
	Missing               bool
	Cases                 []tryCase
}

type tryCase struct {
	Name, Request, Expected, Curl string
	Live                          liveResult
}

func (b *Board) Handler() http.Handler {
	css, _ := webFS.ReadFile("web/style.css")
	b.cssVersion = fmt.Sprintf("%x", sha256.Sum256(css))[:10] // in the URL: a new release is never served a cached stylesheet
	tpl := b.templates()
	render := func(w http.ResponseWriter, name string, d *pageData) {
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, name, d); err != nil { // a rendering bug must not take the board down
			http.Error(w, "render error: "+err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		d := &pageData{Title: b.cfg.Title, Refresh: int(b.cfg.Interval.Seconds()), Board: b, Filter: r.URL.Query().Get("p"),
			Flash: r.URL.Query().Get("flash"), Error: r.URL.Query().Get("error")}
		for _, s := range b.All() {
			if d.Filter == "" || d.Filter == s.P.ID {
				d.States = append(d.States, s)
				for _, r := range s.Runs {
					d.Rows = append(d.Rows, row{s, r})
				}
			}
		}
		d.EnvNames = envNames(d.States)
		sort.SliceStable(d.Rows, func(i, j int) bool { return d.Rows[i].R.Created.After(d.Rows[j].R.Created) })
		render(w, "list.html", d)
	})
	mux.HandleFunc("GET /run/{pipeline}/{id}", func(w http.ResponseWriter, r *http.Request) {
		ps := b.State(r.PathValue("pipeline"))
		if ps == nil {
			http.NotFound(w, r)
			return
		}
		var run *Run
		for _, x := range ps.Runs {
			if x.ID == r.PathValue("id") {
				run = x
			}
		}
		if run == nil {
			http.NotFound(w, r)
			return
		}
		d := &pageData{Title: run.Name, Refresh: 15, Board: b, State: ps, Run: run, States: b.All()}
		if ps.P.Try != nil && len(run.Items) > 0 {
			d.Try = b.tryItems(ps, run)
		}
		render(w, "run.html", d)
	})
	mux.HandleFunc("POST /promote/{pipeline}/{env}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		v, err := b.Promote(r.PathValue("pipeline"), r.PathValue("env"))
		q := url.Values{"p": {r.PathValue("pipeline")}}
		if err != nil {
			q.Set("error", err.Error())
		} else {
			q.Set("flash", fmt.Sprintf("已发起 promote %s → %s,PR 一会儿就会出现(合并后上线)", v, r.PathValue("env")))
		}
		http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.apiState())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		io.WriteString(w, b.metrics())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(css)
	})
	return mux
}

// sameOrigin: the promote form posts from the board itself (no cookies here, but a page elsewhere must not trigger it).
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin"
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (b *Board) tryItems(ps *PipelineState, run *Run) []tryItem {
	t := ps.P.Try
	cat := b.catalog(ps)
	env := ps.P.env(t.Env)
	var out []tryItem
	for _, m := range run.Items {
		a := cat[m]
		if a == nil {
			out = append(out, tryItem{Method: m, Missing: true})
			continue
		}
		it := tryItem{Method: m, Summary: a.Summary, Path: a.Path}
		for _, c := range a.Cases {
			exp := "(没有写预期)"
			if len(c.Response) > 0 && string(c.Response) != "null" {
				exp = prettyJSON(c.Response)
			}
			curl := fmt.Sprintf("curl -s -X POST %s%s \\\n  -H 'Content-Type: application/json' \\\n", env.URL, a.Path)
			if t.CurlToken != "" {
				curl += fmt.Sprintf("  -H \"Authorization: Bearer %s\" \\\n", t.CurlToken)
			}
			curl += fmt.Sprintf("  -d '%s'", compactJSON(c.Request))
			it.Cases = append(it.Cases, tryCase{Name: c.Name, Request: prettyJSON(c.Request), Expected: exp, Curl: curl,
				Live: b.Live(t, a.Path, c.Request)})
		}
		out = append(out, it)
	}
	return out
}

// ── API / metrics ──────────────────────────────────────────────────────────────────────────────────────────────────

type apiRun struct {
	ID, Name, Title, Trigger, TriggerURL, Status string
	Items                                        []string
	Created                                      time.Time
	Steps                                        []Step
}

func (b *Board) apiState() any {
	var out []map[string]any
	for _, s := range b.All() {
		var runs []apiRun
		for _, r := range s.Runs {
			runs = append(runs, apiRun{ID: r.ID, Name: r.Name, Title: r.Title, Trigger: r.Trigger, TriggerURL: r.TriggerURL,
				Status: r.Status(), Items: r.Items, Created: r.Created, Steps: r.Steps})
		}
		out = append(out, map[string]any{"pipeline": s.P.ID, "title": s.P.Title, "updated": s.Updated, "error": s.Error,
			"environments": s.Envs, "gates": s.Gates, "runs": runs})
	}
	return map[string]any{"pipelines": out}
}

var statusValue = map[string]int{Done: 2, Skip: 2, Running: 1, Waiting: 0, Failed: -1, Person: -2}
var labelEsc = regexp.MustCompile(`["\\\n]`)

func (b *Board) metrics() string {
	var w strings.Builder
	w.WriteString("# HELP delivery_run_status 2 done, 1 running, 0 waiting, -1 failed, -2 waiting for a person\n# TYPE delivery_run_status gauge\n")
	for _, s := range b.All() {
		for _, r := range s.Runs {
			fmt.Fprintf(&w, "delivery_run_status{pipeline=%q,run=%q,name=%q} %d\n", s.P.ID, r.ID, labelEsc.ReplaceAllString(trunc(r.Name, 80), ""), statusValue[r.Status()])
		}
	}
	w.WriteString("# HELP delivery_promotion_ready 1 when the gate of a promotion passes\n# TYPE delivery_promotion_ready gauge\n")
	w.WriteString("# HELP delivery_env_behind_commits commits of the branch an environment is behind the one it promotes from\n# TYPE delivery_env_behind_commits gauge\n")
	for _, s := range b.All() {
		for _, g := range s.Gates {
			fmt.Fprintf(&w, "delivery_promotion_ready{pipeline=%q,env=%q} %d\n", s.P.ID, g.Env, map[bool]int{true: 1}[g.Ready])
			if g.Behind >= 0 {
				fmt.Fprintf(&w, "delivery_env_behind_commits{pipeline=%q,env=%q} %d\n", s.P.ID, g.Env, g.Behind)
			}
		}
	}
	w.WriteString("# HELP delivery_board_last_update_age_seconds seconds since the last successful refresh\n# TYPE delivery_board_last_update_age_seconds gauge\n")
	for _, s := range b.All() {
		age := -1.0
		if !s.Updated.IsZero() {
			age = b.now().Sub(s.Updated).Seconds()
		}
		fmt.Fprintf(&w, "delivery_board_last_update_age_seconds{pipeline=%q} %.0f\n", s.P.ID, age)
	}
	return w.String()
}

// ── small helpers ──────────────────────────────────────────────────────────────────────────────────────────────────

func prettyJSON(b []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") != nil {
		return string(b)
	}
	return buf.String()
}

func compactJSON(b []byte) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return string(b)
	}
	return buf.String()
}

func readAll(r io.Reader, max int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	return b
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func stripNL(s string) string { return strings.ReplaceAll(s, "\n", "") }

func envNames(states []*PipelineState) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range states {
		for _, e := range s.P.Environments {
			if !seen[e.Name] {
				seen[e.Name] = true
				out = append(out, e.Name)
			}
		}
	}
	return out
}

type unhealthy struct{ Pipeline, Env, Detail string }

type health struct {
	OK, Total int
	Bad       []unhealthy
}

// Health: the header's summary (only the unhealthy environments are named, however many pipelines there are).
func (b *Board) Health() health {
	var h health
	for _, s := range b.All() {
		for _, e := range s.Envs {
			h.Total++
			if e.Healthy() {
				h.OK++
			} else {
				h.Bad = append(h.Bad, unhealthy{s.P.ID, e.Name, e.Sync + "/" + e.Health + " " + e.Error})
			}
		}
	}
	return h
}
