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
	"slices"
	"sort"
	"strings"
	"time"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

var words = map[string]string{Done: "成功", Running: "进行中", Person: "等人处理", Failed: "失败", Waiting: "未开始", Skip: "跳过",
	Unknown: "无法观测", Superseded: "已被取代"}

// summaryOrder: the statuses the list counts, in the order shown (a superseded run counts only as superseded).
var summaryOrder = []string{Person, Running, Failed, Unknown, Waiting, Superseded}

var svg = map[string]string{ // 16px icons, stroke = currentColor
	Done:       `<path d="M3.5 8.5l3 3 6-7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>`,
	Failed:     `<path d="M4.5 4.5l7 7m0-7l-7 7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Running:    `<path d="M8 2a6 6 0 1 1-6 6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Person:     `<path d="M6 4v8M10 4v8" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Waiting:    `<circle cx="8" cy="8" r="2" fill="currentColor"/>`,
	Skip:       `<path d="M4 8h8" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	Unknown:    `<path d="M6 6.2a2 2 0 1 1 2.6 1.9c-.4.2-.6.5-.6.9v.6" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/><circle cx="8" cy="12" r="1" fill="currentColor"/>`,
	Superseded: `<path d="M3.5 4.5l3.5 3.5-3.5 3.5M8.5 4.5l3.5 3.5-3.5 3.5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>`,
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
			if r.Status() == Superseded {
				return "已包含在 " + r.SupersededBy
			}
			c := r.Current()
			if c == nil {
				return ""
			}
			return strings.TrimSuffix(c.Stage+" · "+c.Name+" · "+c.Note, " · ")
		},
		"lower": strings.ToLower,
		"pct": func(f *float64) string {
			if f == nil {
				return "?"
			}
			return pct(*f)
		},
		"verdictWord": func(v string) string {
			return map[string]string{VerdictPass: "可以", VerdictReason: "需要理由", VerdictFixOnly: "只允许修复"}[v]
		},
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
	Summary  []count  // list page: how many of the rows are in each status (only statuses with rows)
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

type count struct {
	Status string
	N      int
}

// summarize: runs per status, in summaryOrder (the statuses that have runs).
func summarize(runs []*Run) []count {
	n := map[string]int{}
	for _, r := range runs {
		n[r.Status()]++
	}
	var out []count
	for _, st := range summaryOrder {
		if n[st] > 0 {
			out = append(out, count{st, n[st]})
		}
	}
	return out
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
		var runs []*Run
		for _, r := range d.Rows {
			runs = append(runs, r.R)
		}
		d.Summary = summarize(runs)
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
		v, err := b.Promote(r.PathValue("pipeline"), r.PathValue("env"), r.FormValue("change"), r.FormValue("reason"))
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
	// dry run of a promotion: would the gate let it through with this change kind and reason? (cached state; nothing
	// is dispatched). ?change=feature|fix|exception&reason=…
	mux.HandleFunc("GET /api/gate/{pipeline}/{env}", func(w http.ResponseWriter, r *http.Request) {
		ps := b.State(r.PathValue("pipeline"))
		var g *Gate
		if ps != nil {
			g = ps.Gate(r.PathValue("env"))
		}
		if g == nil {
			http.NotFound(w, r)
			return
		}
		change, reason := or(r.URL.Query().Get("change"), ChangeFeature), r.URL.Query().Get("reason")
		ok, why := g.Allows(change, reason)
		// verdict: the gate as a whole (blocked: a check other than the budget fails); budgetVerdict: the budget alone
		verdict, budgetVerdict := "blocked", ""
		if g.Budget != nil {
			budgetVerdict = g.Budget.Verdict
		}
		if g.Ready || g.Overridable {
			verdict = or(budgetVerdict, VerdictPass)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"pipeline": ps.P.ID, "app": ps.P.App, "env": g.Env, "candidate": g.Candidate,
			"verdict": verdict, "budgetVerdict": budgetVerdict, "change": change, "reason": reason, "allowed": ok, "why": why,
			"checks": g.Checks, "budget": g.Budget, "updated": ps.Updated})
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
	SupersededBy                                 string `json:",omitempty"`
	Items                                        []string
	Created                                      time.Time
	Steps                                        []Step
}

func (b *Board) apiState() any {
	var out []map[string]any
	var all []*Run
	for _, s := range b.All() {
		var runs []apiRun
		for _, r := range s.Runs {
			runs = append(runs, apiRun{ID: r.ID, Name: r.Name, Title: r.Title, Trigger: r.Trigger, TriggerURL: r.TriggerURL,
				Status: r.Status(), SupersededBy: r.SupersededBy, Items: r.Items, Created: r.Created, Steps: r.Steps})
		}
		all = append(all, s.Runs...)
		out = append(out, map[string]any{"pipeline": s.P.ID, "title": s.P.Title, "updated": s.Updated, "error": s.Error,
			"environments": s.Envs, "gates": s.Gates, "delivery": s.Delivery, "runs": runs})
	}
	summary := map[string]int{}
	for _, c := range summarize(all) {
		summary[c.Status] = c.N
	}
	return map[string]any{"pipelines": out, "summary": summary}
}

var statusValue = map[string]int{Done: 2, Skip: 2, Running: 1, Waiting: 0, Failed: -1, Person: -2, Unknown: -3, Superseded: 3}
var labelEsc = regexp.MustCompile(`["\\\n]`)

func (b *Board) metrics() string {
	var w strings.Builder
	w.WriteString("# HELP delivery_run_status 2 done, 1 running, 0 waiting, -1 failed, -2 waiting for a person, -3 unknown (environment not observable), 3 superseded by a newer run\n# TYPE delivery_run_status gauge\n")
	for _, s := range b.All() {
		for _, r := range s.Runs {
			fmt.Fprintf(&w, "delivery_run_status{pipeline=%q,run=%q,name=%q} %d\n", s.P.ID, r.ID, labelEsc.ReplaceAllString(trunc(r.Name, 80), ""), statusValue[r.Status()])
		}
	}
	w.WriteString("# HELP delivery_runs runs shown per status (a superseded run counts only as superseded; unknown: delivered by git to an environment the board cannot observe)\n# TYPE delivery_runs gauge\n")
	for _, s := range b.All() {
		n := map[string]int{}
		for _, r := range s.Runs {
			n[r.Status()]++
		}
		for _, st := range append([]string{Done}, summaryOrder...) {
			fmt.Fprintf(&w, "delivery_runs{pipeline=%q,status=%q} %d\n", s.P.ID, st, n[st])
		}
	}
	w.WriteString("# HELP delivery_env_observed 1 when a status source of the environment answers, 0 when its state is unknown\n# TYPE delivery_env_observed gauge\n")
	for _, s := range b.All() {
		for _, e := range s.Envs {
			fmt.Fprintf(&w, "delivery_env_observed{pipeline=%q,env=%q} %d\n", s.P.ID, e.Name, map[bool]int{true: 1}[e.Observed])
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
	w.WriteString("# HELP delivery_error_budget_remaining_ratio lowest share of the error budget left among the app's SLOs in the target environment of a gated promotion (1 untouched, <= 0 exhausted)\n# TYPE delivery_error_budget_remaining_ratio gauge\n")
	w.WriteString("# HELP delivery_promotion_budget_verdict error budget policy for a promotion: 0 pass, 1 needs a reason, 2 fixes and exceptions only\n# TYPE delivery_promotion_budget_verdict gauge\n")
	for _, s := range b.All() {
		for _, g := range s.Gates {
			if g.Budget == nil {
				continue
			}
			if r := g.Budget.Remaining; r != nil {
				fmt.Fprintf(&w, "delivery_error_budget_remaining_ratio{pipeline=%q,env=%q} %g\n", s.P.ID, g.Env, *r)
			}
			fmt.Fprintf(&w, "delivery_promotion_budget_verdict{pipeline=%q,env=%q,level=%q} %d\n", s.P.ID, g.Env, g.Budget.Level,
				map[string]int{VerdictPass: 0, VerdictReason: 1, VerdictFixOnly: 2}[g.Budget.Verdict])
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
	b.doraMetrics(&w)
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

// envNames: the environment columns, in pipeline order: an environment a pipeline adds goes right after the one
// before it in that pipeline (dev, prod + dev, staging, prod → dev, staging, prod).
// envNames: the environment columns, in pipeline order. An environment only some pipelines have goes before the next
// environment of that pipeline that is already a column (dev, prod + dev, staging, prod + qa, prod → dev, staging,
// qa, prod), or after its previous one.
func envNames(states []*PipelineState) []string {
	var out []string
	for _, s := range states {
		envs := s.P.Environments
		for k, e := range envs {
			if slices.Contains(out, e.Name) {
				continue
			}
			at := len(out)
			for _, next := range envs[k+1:] {
				if i := slices.Index(out, next.Name); i >= 0 {
					at = i
					break
				}
			}
			if at == len(out) && k > 0 {
				at = slices.Index(out, envs[k-1].Name) + 1
			}
			out = slices.Insert(out, at, e.Name)
		}
	}
	return out
}

type unhealthy struct{ Pipeline, Env, Detail string }

type health struct {
	OK, Total int
	Bad       []unhealthy
	Unknown   []unhealthy // not observable: neither healthy nor unhealthy, not in Total
}

// Health: the header's summary (only the unhealthy environments are named, however many pipelines there are).
func (b *Board) Health() health {
	var h health
	for _, s := range b.All() {
		for _, e := range s.Envs {
			if !e.Observed {
				h.Unknown = append(h.Unknown, unhealthy{s.P.ID, e.Name, e.Error})
				continue
			}
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
