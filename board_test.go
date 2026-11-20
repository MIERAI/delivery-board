package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHub answers GET paths from a map (any JSON-marshalable value).
type fakeGitHub map[string]any

func (f fakeGitHub) Get(path string, final bool, v any) error {
	x, ok := f[path]
	if !ok {
		return fmt.Errorf("unexpected GET %s", path)
	}
	b, _ := json.Marshal(x)
	return json.Unmarshal(b, v)
}

type fakeKube map[string]any

func (f fakeKube) Get(path string, v any) error {
	x, ok := f[path]
	if !ok {
		return fmt.Errorf("unexpected kube GET %s", path)
	}
	b, _ := json.Marshal(x)
	return json.Unmarshal(b, v)
}

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const testConfig = `
title: test
pipelines:
  - id: app
    repo: o/app
    changes:
      - head: ^design/
        items: ^design/([A-Z]\w+)\.yaml$
        steps:
          - { stage: Design, name: review, workflow: spec-pr, failureNeedsPerson: true, personNote: answer it }
          - { stage: Design, name: merge, merge: true }
          - { stage: Gen, name: generate, workflow: generate }
          - { stage: Gen, name: gen PR, followPR: "^gen-{sha7}$" }
    push: { label: push }
    release: { name: release, jobs: { build: image, deploy: to dev } }
    environments:
      - name: dev
        manifests: { repo: o/m, path: app/dev.yaml, image: img, format: kustomize }
        argocd: { app: app-dev }
        workload: { namespace: dev, name: app }
      - name: prod
        manifests: { repo: o/m, path: app/prod.yaml, image: img, format: kustomize }
        argocd: { app: app-prod }
        workload: { namespace: prod, name: app }
        promotion: { from: dev, soak: 10m, prTitle: ^promote app, workflow: { file: promote.yml, inputs: { app: app } } }
`

// world: a design PR merged → generated PR merged (g1) → released → dev; prod still runs o1.
type world struct {
	gh       fakeGitHub
	kube     fakeKube
	devSince time.Duration // how long dev has run g1
}

func sha(s string) string { return s + strings.Repeat("0", 40-len(s)) }

func kust(tag string) map[string]string {
	y := "images:\n  - name: img\n    newTag: \"" + tag + "\"\n"
	return map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(y))}
}

func newWorld(devSince time.Duration) *world {
	w := &world{gh: fakeGitHub{}, kube: fakeKube{}, devSince: devSince}
	merged, genMerged := t0.Add(-2*time.Hour), t0.Add(-90*time.Minute)
	w.gh["repos/o/app/commits?sha=main&per_page=100"] = []map[string]any{{"sha": sha("g1")}, {"sha": sha("m1")}, {"sha": sha("o1")}}
	w.gh["repos/o/app/pulls?state=all&per_page=50&sort=created&direction=desc"] = []map[string]any{
		{"number": 2, "title": "gen", "state": "closed", "html_url": "u/2", "created_at": merged, "merged_at": genMerged,
			"merge_commit_sha": sha("g1"), "head": map[string]any{"ref": "gen-" + short(sha("m1"))}},
		{"number": 1, "title": "design: Foo", "state": "closed", "html_url": "u/1", "created_at": t0.Add(-3 * time.Hour),
			"merged_at": merged, "merge_commit_sha": sha("m1"), "head": map[string]any{"ref": "design/foo"}},
	}
	w.gh["repos/o/app/pulls/1/files?per_page=100"] = []map[string]any{{"filename": "design/Foo.yaml"}, {"filename": "README.md"}}
	run := func(id int, name, branch, s, event, concl string, at time.Time) map[string]any {
		return map[string]any{"id": id, "name": name, "head_branch": branch, "head_sha": s, "event": event, "status": "completed",
			"conclusion": concl, "html_url": fmt.Sprintf("r/%d", id), "created_at": at, "updated_at": at.Add(time.Minute), "run_started_at": at}
	}
	w.gh["repos/o/app/actions/runs?per_page=100"] = map[string]any{"workflow_runs": []any{
		run(14, "release", "main", sha("g1"), "push", "success", genMerged),
		run(13, "generate", "main", sha("m1"), "push", "success", merged),
		run(12, "spec-pr", "design/foo", sha("d2"), "pull_request", "success", t0.Add(-150*time.Minute)),
		run(11, "spec-pr", "design/foo", sha("d1"), "pull_request", "failure", t0.Add(-170*time.Minute)),
		run(10, "release", "main", sha("o1"), "push", "success", t0.Add(-48*time.Hour)),
	}}
	jobs := func(at time.Time) map[string]any {
		return map[string]any{"jobs": []any{
			map[string]any{"name": "build", "status": "completed", "conclusion": "success", "html_url": "j", "started_at": at, "completed_at": at.Add(time.Minute)},
			map[string]any{"name": "deploy", "status": "completed", "conclusion": "success", "html_url": "j", "started_at": at, "completed_at": at.Add(2 * time.Minute)},
			map[string]any{"name": "lint", "status": "completed", "conclusion": "success", "html_url": "j", "started_at": at, "completed_at": at},
		}}
	}
	w.gh["repos/o/app/actions/runs/14/jobs?per_page=50"] = jobs(genMerged)
	w.gh["repos/o/app/actions/runs/10/jobs?per_page=50"] = jobs(t0.Add(-48 * time.Hour))
	hist := func(env string, versions ...any) {
		var commits []map[string]any
		for i := 0; i < len(versions); i += 2 {
			tag, at := versions[i].(string), versions[i+1].(time.Time)
			c := sha(fmt.Sprintf("c%s%d", env, i))
			commits = append([]map[string]any{{"sha": c, "html_url": "mc/" + c[:6], "commit": map[string]any{"committer": map[string]any{"date": at}}}}, commits...)
			w.gh[fmt.Sprintf("repos/o/m/contents/app/%s.yaml?ref=%s", env, c)] = kust(tag)
		}
		w.gh[fmt.Sprintf("repos/o/m/commits?path=app%%2F%s.yaml&per_page=40", env)] = commits
	}
	hist("dev", short(sha("o1")), t0.Add(-48*time.Hour), short(sha("g1")), t0.Add(-devSince))
	hist("prod", short(sha("o1")), t0.Add(-47*time.Hour))
	w.gh["repos/o/m/pulls?state=open&per_page=30"] = []any{}
	w.workload("dev", short(sha("g1")))
	w.workload("prod", short(sha("o1")))
	return w
}

func (w *world) workload(ns, tag string) {
	w.kube["/apis/apps/v1/namespaces/"+ns+"/deployments/app"] = map[string]any{
		"spec":   map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "img:" + tag}}}}},
		"status": map[string]any{"replicas": 2, "updatedReplicas": 2, "readyReplicas": 2},
	}
	w.kube["/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/app-"+ns] = map[string]any{
		"status": map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"}}}
}

func collectTest(t *testing.T, w *world) *PipelineState {
	t.Helper()
	return collectBudget(t, w, nil)
}

func collectBudget(t *testing.T, w *world, budget func(app, env string) *Budget) *PipelineState {
	t.Helper()
	cfg, err := ParseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := collectPipeline(w.gh, w.kube, cfg.Pipelines[0], 12, func() time.Time { return t0 }, budget)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func stepsOf(r *Run) string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Name+"="+s.Status)
	}
	return strings.Join(out, " ")
}

func runByID(t *testing.T, ps *PipelineState, id string) *Run {
	t.Helper()
	for _, r := range ps.Runs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no run %s in %d runs", id, len(ps.Runs))
	return nil
}

func TestDesignChangeToGate(t *testing.T) {
	ps := collectTest(t, newWorld(time.Hour))
	r := runByID(t, ps, "pr-1")
	want := "review=done merge=done generate=done gen PR=done image=done to dev=done dev 上线=done promote → prod=person prod 上线=waiting"
	if got := stepsOf(r); got != want {
		t.Fatalf("steps\n got %s\nwant %s", got, want)
	}
	if r.Name != "Foo" || r.Steps[0].Note != "2 轮" {
		t.Errorf("name %q, review note %q", r.Name, r.Steps[0].Note)
	}
	if r.Steps[0].Start != t0.Add(-170*time.Minute) {
		t.Errorf("review starts at the first round: %v", r.Steps[0].Start)
	}
	if n := r.Steps[7].Note; n != "可以 promote" {
		t.Errorf("promote note %q", n)
	}
	g := ps.Gate("prod")
	if !g.Ready || g.Candidate != short(sha("g1")) || g.Current != short(sha("o1")) || g.Behind != 1 /* m1 has no release: one version */ {
		t.Fatalf("gate %+v", g)
	}
	// the commit o1 was pushed straight to main: its own run, delivered everywhere
	if got := stepsOf(runByID(t, ps, "c-"+short(sha("o1")))); !strings.HasSuffix(got, "dev 上线=done promote → prod=done prod 上线=done") {
		t.Errorf("push run: %s", got)
	}
	// m1 and g1 belong to the design run: no push runs for them
	if len(ps.Runs) != 2 {
		t.Errorf("runs: %d", len(ps.Runs))
	}
}

func TestGateWaitsForSoak(t *testing.T) {
	ps := collectTest(t, newWorld(3*time.Minute))
	g := ps.Gate("prod")
	if g.Ready {
		t.Fatal("gate passed after 3 minutes in dev (soak 10m)")
	}
	if f := g.failing(); f != "在 dev 运行 ≥ 10m" {
		t.Errorf("failing: %q", f)
	}
	if n := runByID(t, ps, "pr-1").Steps[7].Note; !strings.HasPrefix(n, "等门槛") {
		t.Errorf("promote note %q", n)
	}
}

func TestGateNeedsHealthyFrom(t *testing.T) {
	w := newWorld(time.Hour)
	w.kube["/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/app-dev"] = map[string]any{
		"status": map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Degraded"}}}
	if g := collectTest(t, w).Gate("prod"); g.Ready || g.failing() != "dev 健康" {
		t.Fatalf("gate %+v failing %q", g.Ready, g.failing())
	}
}

func TestOpenPromotePR(t *testing.T) {
	w := newWorld(time.Hour)
	w.gh["repos/o/m/pulls?state=open&per_page=30"] = []any{map[string]any{"number": 9, "title": "promote app → prod", "html_url": "p/9", "state": "open"}}
	ps := collectTest(t, w)
	if g := ps.Gate("prod"); g.Ready || g.OpenPR == nil || g.OpenPR.Number != 9 {
		t.Fatalf("gate %+v", g)
	}
	if s := runByID(t, ps, "pr-1").Steps[7]; s.Status != Person || s.Note != "#9 等合并" || s.URL != "p/9" {
		t.Errorf("promote step %+v", s)
	}
}

func TestReviewFailureWaitsForPerson(t *testing.T) {
	w := newWorld(time.Hour)
	pulls := w.gh["repos/o/app/pulls?state=all&per_page=50&sort=created&direction=desc"].([]map[string]any)
	pulls = append([]map[string]any{{"number": 3, "title": "design: Bar", "state": "open", "html_url": "u/3", "created_at": t0,
		"head": map[string]any{"ref": "design/bar"}}}, pulls...)
	w.gh["repos/o/app/pulls?state=all&per_page=50&sort=created&direction=desc"] = pulls
	w.gh["repos/o/app/pulls/3/files?per_page=100"] = []map[string]any{{"filename": "design/Bar.yaml"}}
	runs := w.gh["repos/o/app/actions/runs?per_page=100"].(map[string]any)["workflow_runs"].([]any)
	runs = append([]any{map[string]any{"id": 20, "name": "spec-pr", "head_branch": "design/bar", "head_sha": sha("b1"), "event": "pull_request",
		"status": "completed", "conclusion": "failure", "created_at": t0, "updated_at": t0}}, runs...)
	w.gh["repos/o/app/actions/runs?per_page=100"] = map[string]any{"workflow_runs": runs}
	r := runByID(t, collectTest(t, w), "pr-3")
	if got := stepsOf(r); !strings.HasPrefix(got, "review=person merge=person") {
		t.Fatalf("steps %s", got)
	}
	if r.Steps[0].Note != "1 轮 · answer it" || r.Status() != Person {
		t.Errorf("note %q status %s", r.Steps[0].Note, r.Status())
	}
}

func TestRollbackAfterArrival(t *testing.T) {
	w := newWorld(time.Hour)
	// prod got g1 and then went back to o1
	commits := []map[string]any{}
	for i, v := range []struct {
		tag string
		at  time.Duration
	}{{short(sha("o1")), 47 * time.Hour}, {short(sha("g1")), 40 * time.Minute}, {short(sha("o1")), 10 * time.Minute}} {
		c := sha(fmt.Sprintf("p%d", i))
		commits = append([]map[string]any{{"sha": c, "html_url": "x", "commit": map[string]any{"committer": map[string]any{"date": t0.Add(-v.at)}}}}, commits...)
		w.gh["repos/o/m/contents/app/prod.yaml?ref="+c] = kust(v.tag)
	}
	w.gh["repos/o/m/commits?path=app%2Fprod.yaml&per_page=40"] = commits
	r := runByID(t, collectTest(t, w), "pr-1")
	last := r.Steps[len(r.Steps)-1]
	if last.Status != Done || last.Note != "之后回滚到 "+short(sha("o1")) {
		t.Fatalf("prod step %+v", last)
	}
}

func TestSyncingEnvironment(t *testing.T) {
	w := newWorld(time.Hour)
	w.workload("dev", short(sha("o1"))) // git says g1, the cluster still runs o1
	r := runByID(t, collectTest(t, w), "pr-1")
	if s := r.Steps[6]; s.Status != Running || !strings.HasPrefix(s.Note, "同步中") {
		t.Fatalf("dev step %+v", s)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, c := range []struct{ name, edit, want string }{
		{"unknown field", "title: test\n", "field nope"},
		{"promotion from a later env", "promotion: { from: dev", "must name an earlier environment"},
		{"step with two kinds", "{ stage: Design, name: merge, merge: true }", "exactly one of"},
	} {
		t.Run(c.name, func(t *testing.T) {
			y := testConfig
			switch c.name {
			case "unknown field":
				y = strings.Replace(y, c.edit, "title: test\nnope: 1\n", 1)
			case "promotion from a later env":
				y = strings.Replace(y, c.edit, "promotion: { from: prod", 1)
			default:
				y = strings.Replace(y, c.edit, "{ stage: Design, name: merge, merge: true, workflow: x }", 1)
			}
			_, err := ParseConfig([]byte(y))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	if _, err := LoadConfig("examples/config.yaml"); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{"ghcr.io/o/app:abc1234": "abc1234", "localhost:5000/app": "", "app:v1@sha256:x": "v1", "app": ""} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPagesRender(t *testing.T) {
	cfg, _ := ParseConfig([]byte(testConfig))
	b := &Board{cfg: cfg, now: func() time.Time { return t0 }, states: map[string]*PipelineState{}, promoted: map[string]time.Time{},
		live: map[string]liveResult{}}
	b.states["app"] = collectTest(t, newWorld(time.Hour))
	h := b.Handler()
	for path, want := range map[string]string{"/": "环境 2/2 健康", "/?p=app": "在 GitHub 上 Promote", "/?q=1": "落后 1 · 可以 promote", "/run/app/pr-1": "promote → prod", "/metrics": `delivery_promotion_ready{pipeline="app",env="prod"} 1`,
		"/api/state": `"Candidate":"g100000"`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, rec.Code, want)
		}
	}
	// promote: refused cross-origin; without a token it explains how to promote instead
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/promote/app/prod", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin promote: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/promote/app/prod", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.Contains(loc, "error=") {
		t.Errorf("promote without token: %d %s", rec.Code, loc)
	}
}

func TestEnvColumnsKeepPipelineOrder(t *testing.T) {
	mk := func(names ...string) *PipelineState {
		p := &Pipeline{}
		for _, n := range names {
			p.Environments = append(p.Environments, &Environment{Name: n})
		}
		return &PipelineState{P: p}
	}
	got := envNames([]*PipelineState{mk("dev", "prod"), mk("dev", "staging", "prod"), mk("qa", "prod")})
	if want := []string{"dev", "staging", "qa", "prod"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// In a monorepo, commits of another service (no release run of this pipeline) do not make it fall behind.
func TestBehindCountsOwnReleasesOnly(t *testing.T) {
	w := newWorld(time.Hour)
	w.gh["repos/o/app/commits?sha=main&per_page=100"] = []map[string]any{{"sha": sha("g1")}, {"sha": sha("x2")}, {"sha": sha("x1")}, {"sha": sha("m1")}, {"sha": sha("o1")}}
	if g := collectTest(t, w).Gate("prod"); g.Behind != 1 {
		t.Fatalf("behind %d, want 1 (x1, x2, m1: other services / no release)", g.Behind)
	}
}

func TestManifestTagFormats(t *testing.T) {
	values := []byte(`# values
image:
  repository: ghcr.io/o/worker
  tag: "abc1234"
sidecar:
  image: { tag: v2, repository: busybox }
deploy:
  version: "x9"
`)
	env := func(format, path, image, key string) *Environment {
		e := &Environment{}
		e.Manifests.Format, e.Manifests.Path, e.Manifests.Image, e.Manifests.Key = format, path, image, key
		return e
	}
	for _, c := range []struct {
		e    *Environment
		raw  []byte
		want string
	}{
		{env("helm", "v.yaml", "ghcr.io/o/worker", ""), values, "abc1234"},
		{env("helm", "v.yaml", "busybox", ""), values, "v2"},
		{env("helm", "v.yaml", "missing", ""), values, ""},
		{env("helm", "v.yaml", "", "deploy.version"), values, "x9"},
		{env("helm", "v.yaml", "", "deploy.nope"), values, ""},
		{env("kustomize", "k/kustomization.yaml", "img", ""), []byte("images:\n  - name: img\n    newTag: \"t1\"\n"), "t1"},
	} {
		if got := manifestTag(c.raw, c.e); got != c.want {
			t.Errorf("%s %s %q: got %q, want %q", c.e.Manifests.Format, c.e.Manifests.Image, c.e.Manifests.Key, got, c.want)
		}
	}
	// the format follows the file name unless given
	cfg, err := ParseConfig([]byte(strings.NewReplacer("path: app/dev.yaml, image: img, format: kustomize", "path: app/values-dev.yaml, image: img",
		"path: app/prod.yaml, image: img, format: kustomize", "path: app/prod/kustomization.yaml, image: img").Replace(testConfig)))
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Pipelines[0].Environments; e[0].Manifests.Format != "helm" || e[1].Manifests.Format != "kustomize" {
		t.Errorf("formats %s %s", e[0].Manifests.Format, e[1].Manifests.Format)
	}
}
