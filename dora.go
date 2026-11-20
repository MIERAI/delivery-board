package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DORA metrics (deployment frequency, lead time for changes, change failure rate, time to restore) from what the
// board reads anyway: the history of each environment's manifests file and the commits of the application's branch.
// No extra GitHub calls. Definitions and blind spots: docs/dora.md.

// DoraWindow: the window of the SLIs the board computes itself (the SLO window of the platform).
const DoraWindow = 28 * 24 * time.Hour

// doraBuckets: upper bounds (seconds) of the lead time and restore time histograms. 86400 (1 day) is the lead time
// SLO's threshold: an SLO threshold must be a bucket edge.
var doraBuckets = []float64{900, 3600, 4 * 3600, 86400, 3 * 86400, 7 * 86400}

// Deployment: the environment's version changed (a commit of its manifests file with another image tag).
type Deployment struct {
	At       time.Time
	Tag, URL string
	Rollback bool          // to a version older than the one it replaced
	LeadTime time.Duration // commit of the version (on the branch) → arrival; -1: unknown (rollback, tag not a recent commit)
	// Failed: a later deployment restored the version before this one (rollback) or reverted a change it brought
	// (revert commit deployed). Restore: from this deployment to that one.
	Failed     bool
	RestoredBy string // URL of the restoring deployment
	Restore    time.Duration
}

// EnvDelivery: the deployments of one environment the history shows, oldest first.
type EnvDelivery struct {
	Env         string
	Deployments []Deployment
	// Since: the oldest version in the history (the baseline: not a deployment itself). Complete: the history reaches
	// the file's first commit; otherwise deployments before Since are not seen.
	Since    time.Time
	Complete bool
}

// branchInfo: the recent commits of the application's branch (newest first, as the board reads them).
type branchInfo struct {
	pos     map[string]int       // short sha → index (0 = newest)
	at      map[string]time.Time // short sha → committer date (the time the change reached the branch)
	reverts map[string]string    // short sha of a revert commit → short sha it reverts
}

var revertRe = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,40})`)

func newBranchInfo(commits []ghCommit) *branchInfo {
	b := &branchInfo{pos: map[string]int{}, at: map[string]time.Time{}, reverts: map[string]string{}}
	for i, cm := range commits {
		s := short(cm.SHA)
		b.pos[s], b.at[s] = i, cm.Commit.Committer.Date
		if msg := cm.Commit.Message; strings.HasPrefix(msg, "Revert ") {
			if m := revertRe.FindStringSubmatch(msg); m != nil {
				b.reverts[s] = short(m[1])
			}
		}
	}
	return b
}

// contains: version tag includes commit sha (both recent commits of the branch).
func (b *branchInfo) contains(sha, tag string) bool {
	a, ok1 := b.pos[short(sha)]
	t, ok2 := b.pos[short(tag)]
	return ok1 && ok2 && t <= a
}

// deployments derives the deployments of one environment from its history (oldest first).
func deployments(env string, hist []Version, complete bool, br *branchInfo) *EnvDelivery {
	out := &EnvDelivery{Env: env, Complete: complete}
	if len(hist) == 0 {
		return out
	}
	out.Since = hist[0].At
	firstSeen := map[string]int{} // tag → index of its first arrival: the version order when the branch does not know a tag
	for i, v := range hist {
		if _, ok := firstSeen[v.Tag]; !ok {
			firstSeen[v.Tag] = i
		}
	}
	newer := func(a, b string) bool { // version a is newer than version b
		pa, ok1 := br.pos[short(a)]
		pb, ok2 := br.pos[short(b)]
		if ok1 && ok2 {
			return pa < pb
		}
		return firstSeen[a] > firstSeen[b]
	}
	deps := []Deployment{}
	cur := hist[0].Tag
	for _, v := range hist[1:] {
		if v.Tag == cur {
			continue // the file changed, the version did not
		}
		d := Deployment{At: v.At, Tag: v.Tag, URL: v.URL, LeadTime: -1, Rollback: newer(cur, v.Tag)}
		if t, ok := br.at[short(v.Tag)]; ok && !t.IsZero() && !d.Rollback && !v.At.Before(t) {
			d.LeadTime = v.At.Sub(t)
		}
		if d.Rollback { // the first of the newer deployments it undoes failed
			first := -1
			for j := len(deps) - 1; j >= 0 && newer(deps[j].Tag, v.Tag); j-- {
				first = j
			}
			if first >= 0 {
				fail(&deps[first], d)
			}
		} else { // a revert commit arriving: the deployment that brought the reverted commit failed
			for r, x := range br.reverts {
				if !br.contains(r, v.Tag) || br.contains(r, cur) {
					continue // not brought by this deployment
				}
				for j := range deps {
					prev := hist[0].Tag
					if j > 0 {
						prev = deps[j-1].Tag
					}
					if br.contains(x, deps[j].Tag) && !br.contains(x, prev) {
						fail(&deps[j], d)
						break
					}
				}
			}
		}
		deps = append(deps, d)
		cur = v.Tag
	}
	out.Deployments = deps
	return out
}

func fail(bad *Deployment, by Deployment) {
	if bad.Failed {
		return // restored earlier
	}
	bad.Failed, bad.RestoredBy, bad.Restore = true, by.URL, by.At.Sub(bad.At)
}

// DoraWindowStats: the SLIs over the window ending at now, by deployment time (a failure counts in the window of the
// deployment that failed, as soon as the restoring deployment is seen).
type DoraWindowStats struct {
	Deployments, Failures int
	Lead, Restore         histogram
	Covered               time.Duration // the part of the window the history covers
}

func (e *EnvDelivery) Window(now time.Time) DoraWindowStats {
	st := DoraWindowStats{Lead: newHistogram(), Restore: newHistogram(), Covered: DoraWindow}
	from := now.Add(-DoraWindow)
	if !e.Complete && e.Since.After(from) {
		st.Covered = now.Sub(e.Since)
	}
	for _, d := range e.Deployments {
		if !d.At.After(from) {
			continue
		}
		st.Deployments++
		if d.LeadTime >= 0 {
			st.Lead.observe(d.LeadTime.Seconds())
		}
		if d.Failed {
			st.Failures++
			st.Restore.observe(d.Restore.Seconds())
		}
	}
	return st
}

// histogram: cumulative counts per doraBuckets edge (+Inf = count).
type histogram struct {
	Buckets []int
	Count   int
	Sum     float64
}

func newHistogram() histogram { return histogram{Buckets: make([]int, len(doraBuckets))} }

func (h *histogram) observe(v float64) {
	for i, le := range doraBuckets {
		if v <= le {
			h.Buckets[i]++
		}
	}
	h.Count++
	h.Sum += v
}

// doraEvents: Prometheus counters of the deployments, failures, lead and restore times the process has seen happen.
// The first successful refresh of a pipeline is the baseline: what the history already shows is not counted again,
// so a restart resets the counters to 0 (Prometheus handles that) instead of replaying the history as new events.
// Events while the board is down are missed by the counters; the window SLIs (from the history) still see them.
type doraEvents struct {
	baseline map[string]bool // pipeline → baseline taken
	seen     map[string]bool // deployment key → counted (or in the baseline)
	failed   map[string]bool // deployment key → its failure counted (or in the baseline)
	counters map[[2]string]*doraCounters
}

type doraCounters struct {
	Deployments, Failures int
	Lead, Restore         histogram
}

func (ev *doraEvents) observe(ps *PipelineState) {
	if ev.baseline == nil {
		ev.baseline, ev.seen, ev.failed, ev.counters = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[[2]string]*doraCounters{}
	}
	id := ps.P.ID
	base := !ev.baseline[id]
	ev.baseline[id] = true
	for _, e := range ps.Delivery {
		k := [2]string{id, e.Env}
		ctr := ev.counters[k]
		if ctr == nil {
			ctr = &doraCounters{Lead: newHistogram(), Restore: newHistogram()}
			ev.counters[k] = ctr
		}
		for _, d := range e.Deployments {
			key := id + "\x00" + e.Env + "\x00" + d.At.UTC().Format(time.RFC3339) + "\x00" + d.Tag
			if !ev.seen[key] {
				ev.seen[key] = true
				if !base {
					ctr.Deployments++
					if d.LeadTime >= 0 {
						ctr.Lead.observe(d.LeadTime.Seconds())
					}
				}
			}
			if d.Failed && !ev.failed[key] {
				ev.failed[key] = true
				if !base {
					ctr.Failures++
					ctr.Restore.observe(d.Restore.Seconds())
				}
			}
		}
	}
}

// doraMetrics writes the DORA metrics: counters of what the process saw happen (rates over hours or days within
// Prometheus' retention), and gauges over the 28-day window recomputed from the history at every scrape (the SLIs:
// independent of restarts and of Prometheus' retention).
func (b *Board) doraMetrics(w io.Writer) {
	states := b.All()
	now := b.now()
	b.mu.RLock()
	defer b.mu.RUnlock()
	type row struct {
		pipeline, env string
		ctr           *doraCounters
		win           *DoraWindowStats
	}
	var rows []row
	for _, s := range states {
		for _, e := range s.Delivery {
			st := e.Window(now)
			rows = append(rows, row{s.P.ID, e.Env, b.events.counters[[2]string{s.P.ID, e.Env}], &st})
		}
	}
	lbl := func(r row) string { return fmt.Sprintf("pipeline=%q,env=%q", r.pipeline, r.env) }
	family := func(name, typ, help string, value func(r row) (float64, bool)) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, r := range rows {
			if v, ok := value(r); ok {
				fmt.Fprintf(w, "%s{%s} %s\n", name, lbl(r), num(v))
			}
		}
	}
	hist := func(name, typ, help string, h func(r row) *histogram) {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
		if typ == "histogram" {
			fmt.Fprintf(w, "# TYPE %s histogram\n", name)
		} else {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				fmt.Fprintf(w, "# TYPE %s%s gauge\n", name, suffix)
			}
		}
		for _, r := range rows {
			x := h(r)
			if x == nil {
				continue
			}
			for i, le := range doraBuckets {
				fmt.Fprintf(w, "%s_bucket{%s,le=%q} %d\n", name, lbl(r), num(le), x.Buckets[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_sum{%s} %s\n%s_count{%s} %d\n", name, lbl(r), x.Count, name, lbl(r), num(x.Sum), name, lbl(r), x.Count)
		}
	}
	ctr := func(f func(c *doraCounters) float64) func(r row) (float64, bool) {
		return func(r row) (float64, bool) {
			if r.ctr == nil {
				return 0, false
			}
			return f(r.ctr), true
		}
	}
	family("delivery_deployments_total", "counter", "deployments (image tag changes in the environment's manifests) seen by this process",
		ctr(func(c *doraCounters) float64 { return float64(c.Deployments) }))
	family("delivery_change_failures_total", "counter", "deployments later rolled back or reverted, counted when the restoring deployment is seen",
		ctr(func(c *doraCounters) float64 { return float64(c.Failures) }))
	hist("delivery_lead_time_seconds", "histogram", "commit of the deployed version (on the branch) to its arrival in the environment; rollbacks excluded",
		func(r row) *histogram {
			if r.ctr == nil {
				return nil
			}
			return &r.ctr.Lead
		})
	hist("delivery_restore_time_seconds", "histogram", "failed deployment to the deployment that restored it",
		func(r row) *histogram {
			if r.ctr == nil {
				return nil
			}
			return &r.ctr.Restore
		})
	family("delivery_window_deployments", "gauge", "deployments in the last 28 days (from the manifests history)",
		func(r row) (float64, bool) { return float64(r.win.Deployments), true })
	family("delivery_window_change_failures", "gauge", "deployments of the last 28 days that a later deployment rolled back or reverted",
		func(r row) (float64, bool) { return float64(r.win.Failures), true })
	hist("delivery_window_lead_time_seconds", "gauge", "lead time of the deployments of the last 28 days (cumulative buckets, as a histogram)",
		func(r row) *histogram { return &r.win.Lead })
	hist("delivery_window_restore_time_seconds", "gauge", "restore time of the failed deployments of the last 28 days (cumulative buckets)",
		func(r row) *histogram { return &r.win.Restore })
	family("delivery_window_covered_seconds", "gauge", "how much of the 28-day window the manifests history covers (less: deployments before it are not seen)",
		func(r row) (float64, bool) { return r.win.Covered.Seconds(), true })
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
