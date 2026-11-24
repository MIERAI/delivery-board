package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncKeepsCharactersWhole(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"交付看板的标题", 7, "交付看板的标题"}, // exactly n characters: not cut, whatever its length in bytes
		{"交付看板的标题很长很长", 5, "交付看板的…"},
		{"fault switches expire — FAULTS_DIR/expires", 23, "fault switches expire…"}, // the word boundary before the em dash
		{"a—b—c—d—e—f", 4, "a—b…"},
		{"deploy 🚀🚀🚀 now", 9, "deploy 🚀🚀…"},
		{"team 👩‍💻 ok", 6, "team…"}, // a ZWJ sequence is not split (👩 + ZWJ + 💻)
		{"café", 3, "caf…"},
		{"café au lait", 4, "caf…"}, // e + combining acute stays together
		{"OpenTelemetry tracing in web, api and worker (#62)", 30, "OpenTelemetry tracing in web,…"},
	} {
		got := trunc(c.in, c.n)
		if got != c.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("trunc(%q, %d) = %q: not valid UTF-8", c.in, c.n, got)
		}
		if utf8.RuneCountInString(got) > c.n+1 {
			t.Errorf("trunc(%q, %d) = %q: longer than %d characters + …", c.in, c.n, got, c.n)
		}
	}
	if got := capitalize("本番"); got != "本番" {
		t.Errorf("capitalize(本番) = %q", got)
	}
	if got := capitalize("dev"); got != "Dev" {
		t.Errorf("capitalize(dev) = %q", got)
	}
}

// The board cannot read dev's cluster (neither the workload nor the Argo CD Application): a version git delivered there
// is unknown, not running ("同步中 · 运行 ?" for ever), and the environment is not counted as unhealthy.
func TestUnobservableEnvironmentIsUnknown(t *testing.T) {
	w := newWorld(time.Hour)
	delete(w.kube, "/apis/apps/v1/namespaces/dev/deployments/app")
	delete(w.kube, "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/app-dev")
	ps := collectTest(t, w)
	if e := ps.Env("dev"); e.Observed || !ps.Env("prod").Observed {
		t.Fatalf("observed: dev %v prod %v", e.Observed, ps.Env("prod").Observed)
	}
	r := runByID(t, ps, "pr-1")
	s := r.Steps[6]
	if s.Name != "dev 上线" || s.Status != Unknown || !strings.HasPrefix(s.Note, "无法观测") {
		t.Fatalf("dev step %+v", s)
	}
	if r.Status() != Unknown { // promote → prod waits on dev's health, which cannot be known
		t.Errorf("run status %s (%s)", r.Status(), stepsOf(r))
	}
	if g := ps.Gate("prod"); g.Ready || g.Checks[1].Detail != "无法观测" {
		t.Errorf("gate ready %v, health check %+v", g.Ready, g.Checks[1])
	}
	b := testBoard(t, ps)
	if h := b.Health(); h.Total != 1 || len(h.Bad) != 0 || len(h.Unknown) != 1 {
		t.Errorf("health %+v", h)
	}
	if m := b.metrics(); !strings.Contains(m, `delivery_env_observed{pipeline="app",env="dev"} 0`) ||
		!strings.Contains(m, `delivery_runs{pipeline="app",status="unknown"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}
}

// An environment declared unobserved is not read at all (no 404s against the board's own cluster).
func TestDeclaredUnobservedEnvironment(t *testing.T) {
	cfg, err := ParseConfig([]byte(strings.Replace(testConfig, "argocd: { app: app-dev }", "argocd: { app: app-dev }\n        unobserved: true", 1)))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(time.Hour)
	ps, err := collectPipeline(w.gh, fakeKube{}, cfg.Pipelines[0], 12, func() time.Time { return t0 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := ps.Env("dev"); e.Observed {
		t.Fatal("dev observed")
	}
	if s := runByID(t, ps, "pr-1").Steps[6]; s.Status != Unknown {
		t.Fatalf("dev step %+v", s)
	}
	// and from an app.yaml
	ps2, err := pipelinesOf([]byte("name: v2\nrepo: o/s\nenvironments:\n  - { name: lab, unobserved: true }\ncomponents: [{ name: v2, kustomize: overlays }]\n"+
		"services: [{ name: a, image: i/a, component: v2 }]\n"), "v2", &Discover{Repo: "o/m"})
	if err != nil || !ps2[0].Environments[0].Unobserved {
		t.Fatalf("app.yaml unobserved: %v %+v", err, ps2)
	}
}

// pushWorld: after the design run's g1, two more commits p1, p2 were released; dev runs p2, prod still o1. Every one of
// g1, p1, p2 could be promoted to prod, but promoting p2 delivers g1 and p1 too.
func pushWorld() *world {
	w := newWorld(time.Hour)
	w.gh["repos/o/app/commits?sha=main&per_page=100"] = []map[string]any{{"sha": sha("p2")}, {"sha": sha("p1")}, {"sha": sha("g1")}, {"sha": sha("m1")}, {"sha": sha("o1")}}
	runs := w.gh["repos/o/app/actions/runs?per_page=100"].(map[string]any)["workflow_runs"].([]any)
	for i, c := range []struct {
		id  int
		sha string
		at  time.Duration
	}{{31, "p1", 80 * time.Minute}, {32, "p2", 70 * time.Minute}} {
		at := t0.Add(-c.at)
		runs = append([]any{map[string]any{"id": c.id, "name": "release", "head_branch": "main", "head_sha": sha(c.sha), "event": "push",
			"status": "completed", "conclusion": "success", "display_title": "change " + c.sha, "created_at": at, "updated_at": at, "run_started_at": at}}, runs...)
		w.gh[map[int]string{0: "repos/o/app/actions/runs/31/jobs?per_page=50", 1: "repos/o/app/actions/runs/32/jobs?per_page=50"}[i]] =
			w.gh["repos/o/app/actions/runs/14/jobs?per_page=50"]
	}
	w.gh["repos/o/app/actions/runs?per_page=100"] = map[string]any{"workflow_runs": runs}
	commits := w.gh["repos/o/m/commits?path=app%2Fdev.yaml&per_page=40"].([]map[string]any)
	c := sha("cdevp2")
	w.gh["repos/o/m/commits?path=app%2Fdev.yaml&per_page=40"] = append([]map[string]any{{"sha": c, "html_url": "mc/p2",
		"commit": map[string]any{"committer": map[string]any{"date": t0.Add(-30 * time.Minute)}}}}, commits...)
	w.gh["repos/o/m/contents/app/dev.yaml?ref="+c] = kust(short(sha("p2")))
	w.workload("dev", short(sha("p2")))
	return w
}

func TestOlderPendingRunsAreSuperseded(t *testing.T) {
	ps := collectTest(t, pushWorld())
	newest := runByID(t, ps, "c-"+short(sha("p2")))
	if got := stepsOf(newest); !strings.HasSuffix(got, "promote → prod=person prod 上线=waiting") || newest.SupersededBy != "" {
		t.Fatalf("newest run: %s (superseded by %q)", got, newest.SupersededBy)
	}
	if n := newest.Steps[3].Note; n != "可以 promote" {
		t.Errorf("newest promote note %q", n)
	}
	for _, id := range []string{"c-" + short(sha("p1")), "pr-1"} {
		r := runByID(t, ps, id)
		if r.Status() != Superseded || r.SupersededBy != short(sha("p2")) {
			t.Errorf("%s: status %s by %q (%s)", id, r.Status(), r.SupersededBy, stepsOf(r))
		}
		for _, s := range r.Steps {
			switch s.Name {
			case "promote → prod", "prod 上线":
				if s.Status != Superseded || s.Note != "已包含在 "+short(sha("p2")) || s.URL != "" {
					t.Errorf("%s %s: %+v", id, s.Name, s)
				}
			default: // what was done stays done
				if s.Status != Done {
					t.Errorf("%s %s: %s", id, s.Name, s.Status)
				}
			}
		}
	}
	if r := runByID(t, ps, "c-"+short(sha("o1"))); r.Status() != Done {
		t.Errorf("delivered run: %s", stepsOf(r))
	}
	// the counts: one promotion waits for a person, two runs are superseded, one is done
	got := map[string]int{}
	for _, c := range summarize(ps.Runs) {
		got[c.Status] = c.N
	}
	if got[Person] != 1 || got[Superseded] != 2 || got[Running] != 0 {
		t.Errorf("summary %v", got)
	}
	b := testBoard(t, ps)
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"等人处理 1", "已被取代 2", `class="row superseded"`, "已包含在 " + short(sha("p2"))} {
		if !strings.Contains(body, want) {
			t.Errorf("list page: missing %q", want)
		}
	}
	rec = httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/state", nil))
	if !strings.Contains(rec.Body.String(), `"summary":{"person":1,"superseded":2}`) {
		t.Errorf("api summary: %s", rec.Body.String()[max(0, len(rec.Body.String())-200):])
	}
}

// A newer run that has not reached the step yet (its release still runs) does not supersede an older one waiting there.
func TestNotSupersededByARunNotThereYet(t *testing.T) {
	w := pushWorld()
	// p2 is not in dev yet: dev still runs p1
	commits := w.gh["repos/o/m/commits?path=app%2Fdev.yaml&per_page=40"].([]map[string]any)
	w.gh["repos/o/m/contents/app/dev.yaml?ref="+commits[0]["sha"].(string)] = kust(short(sha("p1")))
	w.workload("dev", short(sha("p1")))
	ps := collectTest(t, w)
	if r := runByID(t, ps, "c-"+short(sha("p2"))); r.Status() != Waiting {
		t.Fatalf("p2: %s", stepsOf(r))
	}
	if r := runByID(t, ps, "c-"+short(sha("p1"))); r.Status() != Person || r.SupersededBy != "" {
		t.Errorf("p1: %s", stepsOf(r))
	}
	if r := runByID(t, ps, "pr-1"); r.SupersededBy != short(sha("p1")) {
		t.Errorf("pr-1 superseded by %q: %s", r.SupersededBy, stepsOf(r))
	}
}

func testBoard(t *testing.T, ps *PipelineState) *Board {
	t.Helper()
	cfg, _ := ParseConfig([]byte(testConfig))
	b := &Board{cfg: cfg, now: func() time.Time { return t0 }, states: map[string]*PipelineState{"app": ps}, promoted: map[string]time.Time{},
		live: map[string]liveResult{}}
	b.states["app"].P = cfg.Pipelines[0]
	return b
}
