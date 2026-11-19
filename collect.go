package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Status of a step / a run.
const (
	Done    = "done"
	Running = "running"
	Person  = "person" // waiting for a person (review answer, merge, promote)
	Failed  = "failed"
	Waiting = "waiting"
	Skip    = "skip"
)

type Step struct {
	Stage, Name, Status string
	Start, End          time.Time
	URL, Note           string
}

type Run struct {
	ID, Pipeline        string
	Name, Title         string
	Trigger, TriggerURL string
	Items               []string
	Created             time.Time
	Steps               []Step
}

func (r *Run) Status() string { return overall(r.Steps) }

// Current: the first step that is not finished (the last step when all are).
func (r *Run) Current() *Step {
	for i := range r.Steps {
		if s := r.Steps[i].Status; s != Done && s != Skip {
			return &r.Steps[i]
		}
	}
	if len(r.Steps) == 0 {
		return nil
	}
	return &r.Steps[len(r.Steps)-1]
}

// Span: from the first step that started to the last that ended (open while the run is not finished).
func (r *Run) Span() (start, end time.Time) {
	start = r.Created
	for _, s := range r.Steps {
		if t := first(s.Start, s.End); !t.IsZero() {
			start = t
			break
		}
	}
	if st := r.Status(); st == Done || st == Failed {
		for _, s := range r.Steps {
			if s.End.After(end) {
				end = s.End
			}
		}
	}
	return
}

func overall(steps []Step) string {
	has := map[string]bool{}
	for _, s := range steps {
		has[s.Status] = true
	}
	for _, k := range []string{Failed, Running, Person} {
		if has[k] {
			return k
		}
	}
	if has[Waiting] {
		return Waiting
	}
	return Done
}

// Version: one arrival of an image tag in an environment (a commit of the manifests file).
type Version struct {
	At  time.Time
	Tag string
	URL string
}

// Gate: may the version running in From be promoted into Env now?
type Gate struct {
	Env, From          string
	Candidate, Current string    // tag in From (desired by git), tag in Env
	Since              time.Time // Candidate arrived in From
	Behind             int       // releases of the pipeline between Current and Candidate (-1: unknown)
	Checks             []Check
	Ready              bool
	UpToDate           bool // Env already runs From's version: nothing to promote
	OpenPR             *ghPull
	WorkflowURL        string
}

type Check struct {
	Name, Detail string
	OK           bool
}

type PipelineState struct {
	P       *Pipeline
	Runs    []*Run
	Envs    []*EnvState
	Gates   []*Gate
	Updated time.Time
	Error   string
}

func (ps *PipelineState) Env(name string) *EnvState {
	for _, e := range ps.Envs {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func (ps *PipelineState) Gate(env string) *Gate {
	for _, g := range ps.Gates {
		if g.Env == env {
			return g
		}
	}
	return nil
}

type collector struct {
	gh    GitHub
	kube  Kube
	now   func() time.Time
	max   int
	p     *Pipeline
	pos   map[string]int // short sha → index in the branch's commit list (0 = newest)
	pulls []ghPull
	runs  []ghRun
	last  map[string]*ghRun // workflow name + sha → newest run
	envs  map[string]*EnvState
	hist  map[string][]Version
}

func collectPipeline(gh GitHub, k Kube, p *Pipeline, maxRuns int, now func() time.Time) (*PipelineState, error) {
	c := &collector{gh: gh, kube: k, now: now, max: maxRuns, p: p, pos: map[string]int{}, last: map[string]*ghRun{},
		envs: map[string]*EnvState{}, hist: map[string][]Version{}}
	return c.collect()
}

func (c *collector) collect() (*PipelineState, error) {
	p := c.p
	var commits []ghCommit
	if err := c.gh.Get(fmt.Sprintf("repos/%s/commits?sha=%s&per_page=100", p.Repo, url.QueryEscape(p.Branch)), false, &commits); err != nil {
		return nil, err
	}
	for i, cm := range commits {
		c.pos[short(cm.SHA)] = i
	}
	if len(p.Changes) > 0 {
		if err := c.gh.Get(fmt.Sprintf("repos/%s/pulls?state=all&per_page=50&sort=created&direction=desc", p.Repo), false, &c.pulls); err != nil {
			return nil, err
		}
	}
	var runs struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	if err := c.gh.Get(fmt.Sprintf("repos/%s/actions/runs?per_page=100", p.Repo), false, &runs); err != nil {
		return nil, err
	}
	c.runs = runs.Runs
	for i := range c.runs { // newest first: the first run of a workflow for a commit is its latest attempt
		r := &c.runs[i]
		if _, ok := c.last[r.Name+"\x00"+r.HeadSHA]; !ok {
			c.last[r.Name+"\x00"+r.HeadSHA] = r
		}
	}
	ps := &PipelineState{P: p, Updated: c.now()}
	for _, e := range p.Environments {
		st := readEnv(c.kube, e)
		c.envs[e.Name] = st
		ps.Envs = append(ps.Envs, st)
		h, err := c.history(e)
		if err != nil {
			return nil, err
		}
		c.hist[e.Name] = h
	}
	for _, e := range p.Environments {
		if e.Promotion != nil {
			g, err := c.gate(e)
			if err != nil {
				return nil, err
			}
			ps.Gates = append(ps.Gates, g)
		}
	}
	ps.Runs = c.runs2(ps)
	return ps, nil
}

// history: the versions the environment's manifests file went through, oldest first.
func (c *collector) history(e *Environment) ([]Version, error) {
	m := e.Manifests
	var commits []ghCommit
	if err := c.gh.Get(fmt.Sprintf("repos/%s/commits?path=%s&per_page=40", m.Repo, url.QueryEscape(m.Path)), false, &commits); err != nil {
		return nil, err
	}
	var out []Version
	for _, cm := range commits {
		var f struct {
			Content string `json:"content"`
		}
		if err := c.gh.Get(fmt.Sprintf("repos/%s/contents/%s?ref=%s", m.Repo, m.Path, cm.SHA), true, &f); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return nil, err
		}
		if tag := kustomizeTag(raw, m.Image); tag != "" {
			out = append(out, Version{At: cm.Commit.Committer.Date, Tag: tag, URL: cm.HTMLURL})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func kustomizeTag(raw []byte, image string) string {
	var k struct {
		Images []struct {
			Name   string `yaml:"name"`
			NewTag string `yaml:"newTag"`
		} `yaml:"images"`
	}
	if yaml.Unmarshal(raw, &k) != nil {
		return ""
	}
	for _, i := range k.Images {
		if i.Name == image {
			return i.NewTag
		}
	}
	return ""
}

// contains: version tag (a commit of the branch) includes commit sha.
func (c *collector) contains(sha, tag string) bool {
	a, ok1 := c.pos[short(sha)]
	b, ok2 := c.pos[short(tag)]
	return ok1 && ok2 && b <= a
}

func (c *collector) desired(env string) string {
	h := c.hist[env]
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1].Tag
}

func (c *collector) gate(e *Environment) (*Gate, error) {
	pr := e.Promotion
	from := c.envs[pr.From]
	g := &Gate{Env: e.Name, From: pr.From, Candidate: c.desired(pr.From), Current: c.desired(e.Name), Behind: -1,
		WorkflowURL: fmt.Sprintf("https://github.com/%s/actions/workflows/%s", pr.Workflow.Repo, pr.Workflow.File)}
	// arrival of the candidate in From: start of its latest unbroken stretch in the history
	h := c.hist[pr.From]
	for i := len(h) - 1; i >= 0 && h[i].Tag == g.Candidate; i-- {
		g.Since = h[i].At
	}
	g.Behind = c.behind(g.Current, g.Candidate)
	if pr.prTitle != nil {
		var open []ghPull
		if err := c.gh.Get(fmt.Sprintf("repos/%s/pulls?state=open&per_page=30", pr.Workflow.Repo), false, &open); err != nil {
			return nil, err
		}
		for i := range open {
			if pr.prTitle.MatchString(open[i].Title) {
				g.OpenPR = &open[i]
				break
			}
		}
	}
	newer := g.Candidate != "" && g.Candidate != g.Current
	g.UpToDate = g.Candidate != "" && !newer
	g.Checks = append(g.Checks, Check{Name: "有新版本", OK: newer, Detail: map[bool]string{
		true: fmt.Sprintf("%s → %s", or(g.Current, "?"), g.Candidate), false: "已是 " + pr.From + " 的版本"}[newer]})
	healthy := from.Healthy() && from.Tag == g.Candidate
	g.Checks = append(g.Checks, Check{Name: pr.From + " 健康", OK: healthy, Detail: fmt.Sprintf("%s/%s · 运行 %s", from.Sync, from.Health, or(from.Tag, "?"))})
	soaked := !g.Since.IsZero() && c.now().Sub(g.Since) >= pr.Soak.Duration
	g.Checks = append(g.Checks, Check{Name: "在 " + pr.From + " 运行 ≥ " + human(pr.Soak.Duration), OK: soaked,
		Detail: "已运行 " + human(c.now().Sub(g.Since))})
	g.Checks = append(g.Checks, Check{Name: "没有进行中的 promote", OK: g.OpenPR == nil, Detail: openPRDetail(g.OpenPR)})
	g.Ready = true
	for _, ch := range g.Checks {
		g.Ready = g.Ready && ch.OK
	}
	return g, nil
}

// behind: how many versions of this pipeline lie after from up to and including to. A version = a commit with a
// release run (in a monorepo, commits of other services do not count); commits of the branch without a release.
func (c *collector) behind(from, to string) int {
	a, ok1 := c.pos[short(to)]
	b, ok2 := c.pos[short(from)]
	if !ok1 || !ok2 {
		return -1
	}
	if c.p.Release == nil {
		return b - a
	}
	seen := map[string]bool{}
	for _, r := range c.runs {
		if r.Name != c.p.Release.Name || r.HeadBranch != c.p.Branch || seen[r.HeadSHA] {
			continue
		}
		if i, ok := c.pos[short(r.HeadSHA)]; ok && i >= a && i < b {
			seen[r.HeadSHA] = true
		}
	}
	return len(seen)
}

func openPRDetail(p *ghPull) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("#%d 等合并", p.Number)
}

// ── runs ───────────────────────────────────────────────────────────────────────────────────────────────────────────

func (c *collector) runs2(ps *PipelineState) []*Run {
	p := c.p
	var out []*Run
	used := map[string]bool{}
	n := 0
	for i := range c.pulls {
		pr := &c.pulls[i]
		ch := c.changeOf(pr)
		if ch == nil || (pr.State == "closed" && pr.MergedAt == nil) {
			continue
		}
		if n++; n > c.max {
			break
		}
		items := c.items(pr, ch)
		steps, sha := c.changeSteps(pr, ch)
		if sha != "" {
			used[sha] = true
		}
		steps = append(steps, c.delivery(sha, ps)...)
		name := pr.Title
		if len(items) > 0 {
			name = strings.Join(items[:min(2, len(items))], ", ")
			if len(items) > 2 {
				name += fmt.Sprintf(" +%d", len(items)-2)
			}
		}
		out = append(out, &Run{ID: fmt.Sprintf("pr-%d", pr.Number), Pipeline: p.ID, Name: name, Title: pr.Title,
			Trigger: fmt.Sprintf("#%d", pr.Number), TriggerURL: pr.HTMLURL, Items: items, Created: pr.CreatedAt, Steps: steps})
	}
	if p.Push != nil {
		for i := range c.runs {
			r := &c.runs[i]
			if r.Name != p.Release.Name || r.Event != "push" || r.HeadBranch != p.Branch || used[r.HeadSHA] ||
				c.last[r.Name+"\x00"+r.HeadSHA] != r {
				continue
			}
			used[r.HeadSHA] = true
			out = append(out, &Run{ID: "c-" + short(r.HeadSHA), Pipeline: p.ID, Name: or(p.Push.Label, "push") + ": " + trunc(r.DisplayTitle, 70),
				Title: r.DisplayTitle, Trigger: short(r.HeadSHA), TriggerURL: fmt.Sprintf("https://github.com/%s/commit/%s", p.Repo, r.HeadSHA),
				Created: r.CreatedAt, Steps: c.delivery(r.HeadSHA, ps)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	if len(out) > c.max {
		out = out[:c.max]
	}
	return out
}

func (c *collector) changeOf(pr *ghPull) *Change {
	for _, ch := range c.p.Changes {
		if (ch.head != nil && ch.head.MatchString(pr.Head.Ref)) || (ch.title != nil && ch.title.MatchString(pr.Title)) {
			return ch
		}
	}
	return nil
}

func (c *collector) items(pr *ghPull, ch *Change) []string {
	if ch.items == nil {
		return nil
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := c.gh.Get(fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", c.p.Repo, pr.Number), pr.State == "closed", &files); err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, f := range files {
		if m := ch.items.FindStringSubmatch(f.Filename); len(m) > 1 {
			set[m[1]] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// changeSteps follows the change's steps; sha = the commit of the branch it yields ("" = not yet).
func (c *collector) changeSteps(pr *ghPull, ch *Change) (steps []Step, sha string) {
	open := pr.State == "open"
	for _, sd := range ch.Steps {
		switch {
		case sd.Merge:
			if pr.MergedAt == nil {
				return append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Person, URL: pr.HTMLURL, Note: "等合并"}), ""
			}
			steps = append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Done, End: *pr.MergedAt, URL: pr.HTMLURL})
			sha = pr.MergeCommitSHA
		case sd.Workflow != "" && sha == "":
			var rounds []*ghRun // newest first
			for i := range c.runs {
				if r := &c.runs[i]; r.Name == sd.Workflow && r.HeadBranch == pr.Head.Ref {
					rounds = append(rounds, r)
				}
			}
			if len(rounds) == 0 {
				steps = append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: map[bool]string{true: Waiting, false: Skip}[open]})
				continue
			}
			last := rounds[0]
			s := runStep(last, sd.Stage, sd.Name)
			s.Start = rounds[len(rounds)-1].CreatedAt
			s.Note = fmt.Sprintf("%d 轮", len(rounds))
			if s.Status == Failed && open && sd.FailureNeedsPerson {
				s.Status = Person
				if sd.PersonNote != "" {
					s.Note += " · " + sd.PersonNote
				}
			}
			steps = append(steps, s)
		case sd.Workflow != "":
			r := c.last[sd.Workflow+"\x00"+sha]
			if r == nil {
				st := Skip
				if pr.MergedAt != nil && c.now().Sub(*pr.MergedAt) < 3*time.Minute {
					st = Waiting // the run is about to start
				}
				steps = append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: st, Note: map[string]string{Skip: "未触发"}[st]})
				continue
			}
			steps = append(steps, c.jobSteps(r, sd.Stage, sd.Name, sd.Jobs)...)
		case sd.FollowPR != "":
			re := regexp.MustCompile(strings.ReplaceAll(sd.FollowPR, "{sha7}", short(sha)))
			var f *ghPull
			for i := range c.pulls {
				if re.MatchString(c.pulls[i].Head.Ref) {
					f = &c.pulls[i]
					break
				}
			}
			switch {
			case f != nil && f.MergedAt != nil:
				steps = append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Done, End: *f.MergedAt, URL: f.HTMLURL})
				sha = f.MergeCommitSHA
			case f != nil && f.State == "open":
				return append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Person, URL: f.HTMLURL, Note: "等合并"}), ""
			case f != nil:
				return append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Failed, URL: f.HTMLURL, Note: "已关闭"}), ""
			case c.anyEnvContains(sha):
				steps = append(steps, Step{Stage: sd.Stage, Name: sd.Name, Status: Skip, Note: "没有生成 PR"})
			default:
				return append(steps, Step{Stage: sd.Stage, Name: sd.Name}), ""
			}
		}
	}
	return steps, sha
}

func (c *collector) anyEnvContains(sha string) bool {
	for _, e := range c.envs {
		if c.contains(sha, e.Tag) {
			return true
		}
	}
	return false
}

// delivery: release → (promote →) each environment, for commit sha of the branch ("" = placeholders).
func (c *collector) delivery(sha string, ps *PipelineState) []Step {
	p := c.p
	var steps []Step
	if rel := p.Release; rel != nil {
		r := c.last[rel.Name+"\x00"+sha]
		switch {
		case sha == "":
			steps = append(steps, Step{Stage: rel.Stage, Name: rel.Name})
		case r != nil:
			steps = append(steps, c.jobSteps(r, rel.Stage, rel.Name, rel.Jobs)...)
		case c.anyEnvContains(sha):
			steps = append(steps, Step{Stage: rel.Stage, Name: rel.Name, Status: Skip, Note: "随后续 release"})
		default:
			steps = append(steps, Step{Stage: rel.Stage, Name: rel.Name})
		}
	}
	for _, e := range p.Environments {
		var arrived *Version
		if sha != "" {
			for i, v := range c.hist[e.Name] {
				if c.contains(sha, v.Tag) {
					arrived = &c.hist[e.Name][i]
					break
				}
			}
		}
		prev := Waiting
		if len(steps) > 0 {
			prev = steps[len(steps)-1].Status
		}
		if e.Promotion != nil {
			s := Step{Stage: "Promote", Name: "promote → " + e.Name}
			g := ps.Gate(e.Name)
			switch {
			case arrived != nil:
				s.Status, s.End, s.URL = Done, arrived.At, arrived.URL
			case (prev == Done || prev == Skip) && g != nil:
				s.Status = Person
				if g.OpenPR != nil {
					s.URL, s.Note = g.OpenPR.HTMLURL, fmt.Sprintf("#%d 等合并", g.OpenPR.Number)
				} else if g.Ready {
					s.URL, s.Note = g.WorkflowURL, "可以 promote"
				} else {
					s.URL, s.Note = g.WorkflowURL, "等门槛: "+g.failing()
				}
			}
			steps = append(steps, or2(s))
		}
		st := c.envs[e.Name]
		s := Step{Stage: e.Stage, Name: e.Name + " 上线"}
		switch {
		case arrived == nil:
		case !c.contains(sha, c.desired(e.Name)): // arrived, then git went back to an older version
			s.Status, s.Start, s.End, s.URL, s.Note = Done, arrived.At, arrived.At, arrived.URL, "之后回滚到 "+c.desired(e.Name)
		case c.contains(sha, st.Tag) && !st.Healthy() && c.desired(e.Name) == st.Tag:
			s.Status, s.Start, s.Note = Running, arrived.At, st.Sync+"/"+st.Health
		case !c.contains(sha, st.Tag):
			s.Status, s.Start, s.Note = Running, arrived.At, "同步中 · 运行 "+or(st.Tag, "?") // git has it, the cluster not yet
		default:
			s.Status, s.Start, s.End, s.URL, s.Note = Done, arrived.At, arrived.At, arrived.URL, arrived.Tag
		}
		steps = append(steps, or2(s))
	}
	return steps
}

// Failing: the names of the checks that do not pass.
func (g *Gate) Failing() string { return g.failing() }

func (g *Gate) failing() string {
	var out []string
	for _, ch := range g.Checks {
		if !ch.OK {
			out = append(out, ch.Name)
		}
	}
	return strings.Join(out, "、")
}

func or2(s Step) Step {
	if s.Status == "" {
		s.Status = Waiting
	}
	return s
}

func (c *collector) jobSteps(r *ghRun, stage, fallback string, names map[string]string) []Step {
	if len(names) == 0 {
		return []Step{runStep(r, stage, fallback)}
	}
	var jobs struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := c.gh.Get(fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=50", c.p.Repo, r.ID), r.Status == "completed", &jobs); err != nil {
		return []Step{runStep(r, stage, fallback)}
	}
	var out []Step
	for _, j := range jobs.Jobs {
		name, ok := names[j.Name]
		if !ok {
			continue
		}
		s := Step{Stage: stage, Name: name, Status: conclusion(j.Status, j.Conclusion), Start: j.StartedAt, URL: j.HTMLURL}
		if j.Status == "completed" {
			s.End = j.CompletedAt
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []Step{runStep(r, stage, fallback)}
	}
	return out
}

func runStep(r *ghRun, stage, name string) Step {
	s := Step{Stage: stage, Name: name, Status: conclusion(r.Status, r.Conclusion), Start: first(r.RunStartedAt, r.CreatedAt), URL: r.HTMLURL}
	if r.Status == "completed" {
		s.End = r.UpdatedAt
	}
	return s
}

func conclusion(status, concl string) string {
	if status != "completed" {
		return Running
	}
	switch concl {
	case "success":
		return Done
	case "skipped", "neutral":
		return Skip
	}
	return Failed
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func first(t ...time.Time) time.Time {
	for _, x := range t {
		if !x.IsZero() {
			return x
		}
	}
	return time.Time{}
}

func human(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 0:
		return "0s"
	case s >= 86400:
		return fmt.Sprintf("%dd %dh", s/86400, s%86400/3600)
	case s >= 3600:
		return strings.TrimSuffix(fmt.Sprintf("%dh %dm", s/3600, s%3600/60), " 0m")
	case s >= 60:
		return strings.TrimSuffix(fmt.Sprintf("%dm %ds", s/60, s%60), " 0s")
	}
	return fmt.Sprintf("%ds", s)
}
