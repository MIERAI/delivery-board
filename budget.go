package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Error budget gate: the error budget policy of gitops-platform-blueprint docs/slo.md §6, applied to promotions into
// the gated environments (prod). Design and decisions: docs/error-budget.md.
//
//	budget left (28d, the lowest of the app's SLOs in the target environment)
//	  > 25%                       pass
//	  0–25%                       pass with a stated reason
//	  ≤ 0, or a fast burn now     fixes / reliability work only (change: fix, with a reason) or an explicit exception
//	  unknown (no SLO, no data,   fail closed, like exhausted: a broken SLI or monitoring outage must not open the gate
//	  Prometheus unreachable)     silently; fixes and explicit exceptions still ship
//
// The numbers come from Pyrra's generic recording rules (pyrra --generic-rules): pyrra_objective and
// pyrra_availability{app, env, slo} (availability over the SLO's window), and its multi-window burn-rate alerts
// (ErrorBudgetBurn, long="1h": the fast-burn tier that pages).
type ErrorBudget struct {
	Prometheus   string   `yaml:"prometheus"`   // base URL, e.g. http://kps-prometheus.monitoring.svc:9090
	Environments []string `yaml:"environments"` // promotions into these are gated; default [prod]
	ReasonBelow  float64  `yaml:"reasonBelow"`  // percent of the budget left at or below which a reason is needed; default 25
	Inputs       struct {
		Change string `yaml:"change"` // the promotion workflow's input for the change kind; default change
		Reason string `yaml:"reason"` // … and for the stated reason; default reason
	} `yaml:"inputs"`
}

func (e *ErrorBudget) validate() error {
	if e.Prometheus == "" {
		return fmt.Errorf("errorBudget.prometheus required")
	}
	if _, err := url.Parse(e.Prometheus); err != nil {
		return fmt.Errorf("errorBudget.prometheus: %w", err)
	}
	if len(e.Environments) == 0 {
		e.Environments = []string{"prod"}
	}
	if e.ReasonBelow == 0 {
		e.ReasonBelow = 25
	}
	if e.ReasonBelow < 0 || e.ReasonBelow > 100 {
		return fmt.Errorf("errorBudget.reasonBelow: a percentage")
	}
	e.Inputs.Change = or(e.Inputs.Change, "change")
	e.Inputs.Reason = or(e.Inputs.Reason, "reason")
	return nil
}

func (e *ErrorBudget) gates(env string) bool {
	for _, n := range e.Environments {
		if n == env {
			return true
		}
	}
	return false
}

// Change kinds a promotion declares (workflow input / board form / "Change:" line of the promotion PR).
const (
	ChangeFeature   = "feature"   // the default
	ChangeFix       = "fix"       // a fix or reliability work (a rollback is one)
	ChangeException = "exception" // §6 exception: budget spent by a dependency outside our control, a bad SLI
)

var changeKinds = []string{ChangeFeature, ChangeFix, ChangeException}

// Budget levels, worst last.
const (
	BudgetOK        = "ok"
	BudgetLow       = "low"       // at or below reasonBelow
	BudgetUnknown   = "unknown"   // no SLO, an SLO without data, Prometheus unreachable
	BudgetExhausted = "exhausted" // ≤ 0
	BudgetFastBurn  = "fast-burn" // a page-level (fast) burn alert is firing in the environment now
)

// Verdicts of the gate (what a promotion needs).
const (
	VerdictPass    = "pass"
	VerdictReason  = "needs-reason"
	VerdictFixOnly = "fix-only"
)

// SLOBudget: one SLO of the app in the environment.
type SLOBudget struct {
	SLO       string
	Objective float64  // 0.995
	Remaining *float64 // share of the budget left: 1 untouched, ≤ 0 exhausted; nil = no data
	FastBurn  bool
}

// Budget: the error budget of an app in an environment, and what the policy makes of it.
type Budget struct {
	App, Env  string
	SLOs      []SLOBudget
	Error     string   // Prometheus could not be asked
	Remaining *float64 // the lowest of the SLOs' (nil: none known)
	Worst     string   // the SLO with the lowest
	Level     string
	Verdict   string
	Detail    string // one line for people
}

// judge sets Level, Verdict, Remaining, Worst and Detail from SLOs / Error.
func (b *Budget) judge(reasonBelow float64) {
	b.Remaining, b.Worst = nil, ""
	var unknown, fast []string
	for _, s := range b.SLOs {
		if s.FastBurn {
			fast = append(fast, s.SLO)
		}
		if s.Remaining == nil {
			unknown = append(unknown, s.SLO)
			continue
		}
		if b.Remaining == nil || *s.Remaining < *b.Remaining {
			v := *s.Remaining
			b.Remaining, b.Worst = &v, s.SLO
		}
	}
	low := ""
	if b.Remaining != nil {
		low = fmt.Sprintf("剩余 %s(%s)", pct(*b.Remaining), b.Worst)
	}
	switch {
	case b.Error != "":
		b.Level, b.Detail = BudgetUnknown, "预算未知: Prometheus "+trunc(b.Error, 120)
	case len(fast) > 0:
		b.Level, b.Detail = BudgetFastBurn, "正在快速消耗("+strings.Join(fast, ", ")+"): 先回滚"
	case b.Remaining != nil && *b.Remaining <= 0:
		b.Level, b.Detail = BudgetExhausted, "预算耗尽: "+low
	case len(b.SLOs) == 0:
		b.Level, b.Detail = BudgetUnknown, "预算未知: "+b.App+" 在 "+b.Env+" 没有 SLO"
	case len(unknown) > 0:
		b.Level, b.Detail = BudgetUnknown, "预算未知: 没有数据 "+strings.Join(unknown, ", ")
	case *b.Remaining <= reasonBelow/100:
		b.Level, b.Detail = BudgetLow, low
	default:
		b.Level, b.Detail = BudgetOK, low
	}
	b.Verdict = map[string]string{BudgetOK: VerdictPass, BudgetLow: VerdictReason}[b.Level]
	if b.Verdict == "" {
		b.Verdict = VerdictFixOnly
	}
}

// Allows: may a promotion of this kind, with this reason, pass the budget? why says what is missing.
func (b *Budget) Allows(change, reason string) (ok bool, why string) {
	change, reason = or(strings.TrimSpace(change), ChangeFeature), strings.TrimSpace(reason)
	if !validChange(change) {
		return false, fmt.Sprintf("change 只能是 %s", strings.Join(changeKinds, " / "))
	}
	switch b.Verdict {
	case VerdictPass:
		return true, ""
	case VerdictReason:
		if reason == "" {
			return false, "错误预算 " + b.Detail + ": promote 到 " + b.Env + " 需要写明理由(reason)"
		}
		return true, ""
	}
	if change == ChangeFeature {
		return false, "错误预算 " + b.Detail + ": 只允许修复 / 可靠性改进(change: fix)或明确的例外(change: exception),都要写理由"
	}
	if reason == "" {
		return false, "change: " + change + " 需要写明理由(reason)"
	}
	return true, ""
}

func validChange(c string) bool {
	for _, k := range changeKinds {
		if c == k {
			return true
		}
	}
	return false
}

func pct(f float64) string { return strconv.FormatFloat(math.Round(f*1000)/10, 'f', -1, 64) + "%" }

// ── Prometheus ─────────────────────────────────────────────────────────────────────────────────────────────────────

type promSample struct {
	Metric map[string]string
	Value  float64
}

func promQuery(client *http.Client, base, q string) ([]promSample, error) {
	resp, err := client.Get(strings.TrimRight(base, "/") + "/api/v1/query?query=" + url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", resp.Status, err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("%s: %s", resp.Status, r.Error)
	}
	var out []promSample
	for _, x := range r.Data.Result {
		s, _ := x.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64) // "NaN", "+Inf" parse too: callers check
		if err != nil {
			continue
		}
		out = append(out, promSample{x.Metric, v})
	}
	return out, nil
}

// fetchBudgets: the budgets of every app in the gated environments, keyed app/env (judged).
func fetchBudgets(client *http.Client, cfg *ErrorBudget) (map[string]*Budget, error) {
	var quoted []string
	for _, e := range cfg.Environments {
		quoted = append(quoted, regexp.QuoteMeta(e))
	}
	sel := fmt.Sprintf(`env=~%q`, strings.Join(quoted, "|"))
	objectives, err := promQuery(client, cfg.Prometheus, "pyrra_objective{"+sel+"}")
	if err != nil {
		return nil, err
	}
	avail, err := promQuery(client, cfg.Prometheus, "pyrra_availability{"+sel+"}")
	if err != nil {
		return nil, err
	}
	// the fast-burn tier (14× over 1h and 5m): the alert that pages
	burning, err := promQuery(client, cfg.Prometheus, `ALERTS{alertname="ErrorBudgetBurn", alertstate="firing", long="1h"}`)
	if err != nil {
		return nil, err
	}
	return budgetsFrom(objectives, avail, burning, cfg), nil
}

func budgetsFrom(objectives, avail, burning []promSample, cfg *ErrorBudget) map[string]*Budget {
	type key struct{ app, env, slo string }
	av := map[key]float64{}
	for _, s := range avail {
		k := key{s.Metric["app"], s.Metric["env"], s.Metric["slo"]}
		if old, ok := av[k]; !ok || s.Value < old { // duplicates: the worse one
			av[k] = s.Value
		}
	}
	fast := map[[2]string]bool{} // namespace, slo
	for _, s := range burning {
		fast[[2]string{s.Metric["namespace"], s.Metric["slo"]}] = true
	}
	out := map[string]*Budget{}
	for _, s := range objectives {
		app, env, slo := s.Metric["app"], s.Metric["env"], s.Metric["slo"]
		if app == "" || env == "" || slo == "" {
			continue
		}
		b := out[app+"/"+env]
		if b == nil {
			b = &Budget{App: app, Env: env}
			out[app+"/"+env] = b
		}
		sb := SLOBudget{SLO: slo, Objective: s.Value, FastBurn: fast[[2]string{app + "-" + env, slo}]}
		if a, ok := av[key{app, env, slo}]; ok && s.Value < 1 {
			r := (a - s.Value) / (1 - s.Value)
			if !math.IsNaN(r) && !math.IsInf(r, 0) {
				sb.Remaining = &r
			}
		}
		b.SLOs = append(b.SLOs, sb)
	}
	for _, b := range out {
		sort.Slice(b.SLOs, func(i, j int) bool { return b.SLOs[i].SLO < b.SLOs[j].SLO })
		b.judge(cfg.ReasonBelow)
	}
	return out
}

// refreshBudgets asks Prometheus once for every app (before the pipelines are collected).
func (b *Board) refreshBudgets() {
	eb := b.cfg.ErrorBudget
	if eb == nil {
		return
	}
	client := b.prom
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	m, err := fetchBudgets(client, eb)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.budgetErr = err.Error()
		return // keep nothing: an old number must not pass the gate
	}
	b.budgets, b.budgetErr = m, ""
}

// budget: the judged budget of app in env (a copy), nil when promotions into env are not gated.
func (b *Board) budget(app, env string) *Budget {
	eb := b.cfg.ErrorBudget
	if eb == nil || !eb.gates(env) {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := &Budget{App: app, Env: env}
	switch {
	case b.budgetErr != "":
		out.Error = b.budgetErr
	case b.budgets == nil:
		out.Error = "还没有查询过"
	case b.budgets[app+"/"+env] != nil:
		out.SLOs = append([]SLOBudget(nil), b.budgets[app+"/"+env].SLOs...)
	}
	out.judge(eb.ReasonBelow)
	return out
}

// ── the promotion PR ───────────────────────────────────────────────────────────────────────────────────────────────

// The promotion workflow writes the declared change kind and reason into the PR body (gitops-manifests promote.yml):
//
//	Change: fix
//	Reason: rpc-latency budget spent by the missing index; this adds it
var (
	prChange = regexp.MustCompile(`(?m)^Change:[ \t]*(\S+)`)
	prReason = regexp.MustCompile(`(?m)^Reason:[ \t]*(.*?)[ \t]*\r?$`)
)

func promotionDeclared(body string) (change, reason string) {
	if m := prChange.FindStringSubmatch(body); m != nil {
		change = m[1]
	}
	if m := prReason.FindStringSubmatch(body); m != nil {
		reason = m[1]
	}
	return or(change, ChangeFeature), reason
}

// applyBudget adds the error budget check to a gate. The other checks keep their meaning; Overridable says that the
// budget is the only thing in the way and a promotion stating a reason (and, below zero, being a fix) may pass.
func applyBudget(g *Gate, bud *Budget) {
	if bud == nil {
		return
	}
	g.Budget = bud
	g.Checks = append(g.Checks, Check{Name: "错误预算", OK: bud.Verdict == VerdictPass, Detail: bud.Detail})
	others := true
	for _, ch := range g.Checks[:len(g.Checks)-1] {
		others = others && ch.OK
	}
	g.Ready = others && bud.Verdict == VerdictPass
	g.Overridable = others && !g.Ready
	if g.OpenPR != nil {
		change, reason := promotionDeclared(g.OpenPR.Body)
		g.OpenPRChange, g.OpenPRReason = change, reason
		if ok, why := bud.Allows(change, reason); !ok {
			g.OpenPRPolicy = why
		}
	}
}

// Allows: may a promotion declaring change and reason pass this gate now? (The board's Promote and its dry run.)
func (g *Gate) Allows(change, reason string) (bool, string) {
	if g.Ready && g.Budget == nil {
		return true, ""
	}
	if !g.Ready && !g.Overridable {
		return false, "门槛没通过: " + g.failing()
	}
	return g.Budget.Allows(change, reason)
}

// Need: what a promotion needs right now, for people ("" = nothing beyond the gate).
func (g *Gate) Need() string {
	if g.Budget == nil {
		return ""
	}
	switch g.Budget.Verdict {
	case VerdictReason:
		return "需要理由"
	case VerdictFixOnly:
		return "只允许修复"
	}
	return ""
}
