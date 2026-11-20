package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// branch: commits newest first, each "name@hours before t0[@message]".
func branch(specs ...string) *branchInfo {
	var commits []ghCommit
	for _, s := range specs {
		parts := strings.SplitN(s, "@", 3)
		var c ghCommit
		c.SHA = sha(parts[0])
		var h int
		fmt.Sscan(parts[1], &h)
		c.Commit.Committer.Date = t0.Add(-time.Duration(h) * time.Hour)
		if len(parts) == 3 {
			c.Commit.Message = parts[2]
		}
		commits = append(commits, c)
	}
	return newBranchInfo(commits)
}

// versions: "tag@hours before t0", oldest first; the tag is a commit name of branch().
func versions(specs ...string) []Version {
	var out []Version
	for i, s := range specs {
		name, hs, _ := strings.Cut(s, "@")
		var h int
		fmt.Sscan(hs, &h)
		out = append(out, Version{At: t0.Add(-time.Duration(h) * time.Hour), Tag: short(sha(name)), URL: fmt.Sprintf("m/%d", i)})
	}
	return out
}

func summary(e *EnvDelivery) string {
	var out []string
	for _, d := range e.Deployments {
		s := strings.TrimRight(d.Tag, "0")
		if d.Rollback {
			s += " rollback"
		}
		if d.LeadTime >= 0 {
			s += " lead=" + d.LeadTime.String()
		}
		if d.Failed {
			s += " FAILED restore=" + d.Restore.String() + " by " + d.RestoredBy
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func TestDeployments(t *testing.T) {
	br := branch("e@20", "d@30", "c@40", "b@50", "a@60")
	for _, c := range []struct {
		name string
		hist []Version
		want string
	}{
		{"forward only; the baseline and unchanged versions are not deployments",
			versions("a@59", "b@48", "b@47", "d@27"),
			"b lead=2h0m0s; d lead=3h0m0s"},
		{"rollback: the deployment it undoes failed, restore = until the rollback",
			versions("a@59", "b@48", "c@38", "b@36", "d@27"),
			"b lead=2h0m0s; c lead=2h0m0s FAILED restore=2h0m0s by m/3; b rollback; d lead=3h0m0s"},
		{"rollback over two bad deployments: the first of them failed",
			versions("a@59", "c@38", "d@28", "a@26"),
			"c lead=2h0m0s FAILED restore=12h0m0s by m/3; d lead=2h0m0s; a rollback"},
		{"rollback, roll forward to the same version, rollback again: two failures",
			versions("a@59", "b@48", "a@46", "b@44", "a@40"),
			"b lead=2h0m0s FAILED restore=2h0m0s by m/2; a rollback; b lead=6h0m0s FAILED restore=4h0m0s by m/4; a rollback"},
	} {
		if got := summary(deployments("prod", c.hist, false, br)); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// Tags the branch does not know (semver, or older than its last 100 commits): the order of first arrival decides.
func TestRollbackOfUnknownTags(t *testing.T) {
	hist := []Version{{At: t0.Add(-5 * time.Hour), Tag: "v1.0.1"}, {At: t0.Add(-4 * time.Hour), Tag: "v1.1.1", URL: "u1"},
		{At: t0.Add(-3 * time.Hour), Tag: "v1.0.1", URL: "u2"}, {At: t0.Add(-2 * time.Hour), Tag: "v1.1.1", URL: "u3"}}
	got := summary(deployments("prod", hist, false, branch("x@1")))
	if want := "v1.1.1 FAILED restore=1h0m0s by u2; v1.0.1 rollback; v1.1.1"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRevertFailsTheDeploymentThatBroughtTheCommit(t *testing.T) {
	br := branch(`r@10@Revert "add b"`+"\n\nThis reverts commit "+sha("b")+".", "c@30", "b@50", "a@60")
	got := summary(deployments("prod", versions("a@59", "b@48", "c@28", "r@8"), false, br))
	if want := "b lead=2h0m0s FAILED restore=40h0m0s by m/3; c lead=2h0m0s; r lead=2h0m0s"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// reverted before it reached the environment: the commit and its revert arrive together, nothing failed there
	got = summary(deployments("prod", versions("a@59", "r@8"), false, br))
	if want := "r lead=2h0m0s"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// a revert without the "This reverts commit" line is not attributed
	br = branch(`r@10@Revert "add b"`, "b@50", "a@60")
	if got = summary(deployments("prod", versions("a@59", "b@48", "r@8"), false, br)); strings.Contains(got, "FAILED") {
		t.Fatalf("got %s", got)
	}
}

func TestWindow(t *testing.T) {
	day := 24 * time.Hour
	br := branch("c@1", "b@2", "a@3")
	hist := []Version{{At: t0.Add(-40 * day), Tag: short(sha("a"))}, {At: t0.Add(-30 * day), Tag: short(sha("b")), URL: "1"},
		{At: t0.Add(-time.Hour), Tag: short(sha("c")), URL: "2"}, {At: t0.Add(-30 * time.Minute), Tag: short(sha("b")), URL: "3"}}
	e := deployments("prod", hist, false, br)
	st := e.Window(t0)
	// b (30 days ago) is outside; c (lead 0, failed: rolled back to b after 30m) and the rollback are inside
	if st.Deployments != 2 || st.Failures != 1 || st.Lead.Count != 1 || st.Restore.Count != 1 || st.Restore.Buckets[0] != 0 || st.Restore.Buckets[1] != 1 {
		t.Fatalf("%+v", st)
	}
	if st.Covered != DoraWindow {
		t.Fatalf("covered %s", st.Covered)
	}
	// a truncated history that starts 10 days ago covers 10 of the 28 days; a complete one covers all of them
	e.Since = t0.Add(-10 * day)
	if st = e.Window(t0); st.Covered != 10*day {
		t.Fatalf("covered %s", st.Covered)
	}
	e.Complete = true
	if st = e.Window(t0); st.Covered != DoraWindow {
		t.Fatalf("covered %s", st.Covered)
	}
}

// The counters start at 0: what the first refresh sees is the baseline; later deployments and failures count once.
func TestEventCounters(t *testing.T) {
	br := branch("c@10", "b@20", "a@30")
	ps := func(h []Version) *PipelineState {
		return &PipelineState{P: &Pipeline{ID: "app"}, Delivery: []*EnvDelivery{deployments("prod", h, false, br)}}
	}
	var ev doraEvents
	ev.observe(ps(versions("a@29", "b@18")))
	ctr := ev.counters[[2]string{"app", "prod"}]
	if ctr.Deployments != 0 || ctr.Failures != 0 {
		t.Fatalf("baseline counted: %+v", ctr)
	}
	ev.observe(ps(versions("a@29", "b@18", "c@6")))
	ev.observe(ps(versions("a@29", "b@18", "c@6"))) // same history again
	if ctr.Deployments != 1 || ctr.Lead.Count != 1 || ctr.Lead.Buckets[2] != 1 || ctr.Lead.Buckets[1] != 0 {
		t.Fatalf("after c: %+v", ctr)
	}
	ev.observe(ps(versions("a@29", "b@18", "c@6", "b@5"))) // rollback: c failed
	if ctr.Deployments != 2 || ctr.Failures != 1 || ctr.Restore.Count != 1 || ctr.Restore.Sum != 3600 {
		t.Fatalf("after rollback: %+v", ctr)
	}
	// a restart: a new baseline, nothing replayed
	var ev2 doraEvents
	ev2.observe(ps(versions("a@29", "b@18", "c@6", "b@5")))
	if c := ev2.counters[[2]string{"app", "prod"}]; c.Deployments != 0 || c.Failures != 0 {
		t.Fatalf("restart replayed the history: %+v", c)
	}
}

// End to end through the collector (fake GitHub): dev went o1 → g1, prod rolled g1 back to o1.
func TestDoraMetricsExposed(t *testing.T) {
	w := newWorld(time.Hour)
	w.gh["repos/o/app/commits?sha=main&per_page=100"] = []map[string]any{
		{"sha": sha("g1"), "commit": map[string]any{"committer": map[string]any{"date": t0.Add(-90 * time.Minute)}}},
		{"sha": sha("m1")}, {"sha": sha("o1")}}
	commits := []map[string]any{}
	for i, v := range []struct {
		tag string
		at  time.Duration
	}{{short(sha("o1")), 47 * time.Hour}, {short(sha("g1")), 40 * time.Minute}, {short(sha("o1")), 10 * time.Minute}} {
		c := sha(fmt.Sprintf("p%d", i))
		commits = append([]map[string]any{{"sha": c, "html_url": "x" + c[:2], "commit": map[string]any{"committer": map[string]any{"date": t0.Add(-v.at)}}}}, commits...)
		w.gh["repos/o/m/contents/app/prod.yaml?ref="+c] = kust(v.tag)
	}
	w.gh["repos/o/m/commits?path=app%2Fprod.yaml&per_page=40"] = commits
	cfg, _ := ParseConfig([]byte(testConfig))
	b := &Board{cfg: cfg, now: func() time.Time { return t0 }, states: map[string]*PipelineState{}}
	ps := collectTest(t, w)
	b.states["app"] = ps
	b.events.observe(ps)
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`delivery_deployments_total{pipeline="app",env="prod"} 0`, // the baseline is not counted
		`delivery_window_deployments{pipeline="app",env="dev"} 1`,
		`delivery_window_deployments{pipeline="app",env="prod"} 2`,
		`delivery_window_change_failures{pipeline="app",env="prod"} 1`,
		`delivery_window_change_failures{pipeline="app",env="dev"} 0`,
		`delivery_window_lead_time_seconds_bucket{pipeline="app",env="dev",le="3600"} 1`,  // g1 committed 90m before, in dev 60m before: 30m
		`delivery_window_lead_time_seconds_bucket{pipeline="app",env="prod",le="3600"} 1`, // 50m
		`delivery_window_lead_time_seconds_count{pipeline="app",env="prod"} 1`,            // the rollback has no lead time
		`delivery_window_restore_time_seconds_sum{pipeline="app",env="prod"} 1800`,
		`delivery_window_covered_seconds{pipeline="app",env="prod"} 2419200`,
		"# TYPE delivery_lead_time_seconds histogram",
		"# TYPE delivery_window_lead_time_seconds_bucket gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
