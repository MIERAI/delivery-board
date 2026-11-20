package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func budgetOf(slos ...SLOBudget) *Budget {
	b := &Budget{App: "app", Env: "prod", SLOs: slos}
	b.judge(25)
	return b
}

// §6: > 25% normal; 0–25% a reason; exhausted fixes only; fast burn roll back first; unknown fails closed.
func TestBudgetLevels(t *testing.T) {
	for _, c := range []struct {
		name           string
		b              *Budget
		level, verdict string
		worst          string
	}{
		{"all healthy", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.9)}, SLOBudget{SLO: "b", Remaining: f(0.3)}), BudgetOK, VerdictPass, "b"},
		{"25% is not over 25%", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.25)}), BudgetLow, VerdictReason, "a"},
		{"low", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.9)}, SLOBudget{SLO: "b", Remaining: f(0.01)}), BudgetLow, VerdictReason, "b"},
		{"zero is exhausted", budgetOf(SLOBudget{SLO: "a", Remaining: f(0)}), BudgetExhausted, VerdictFixOnly, "a"},
		{"overspent", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.8)}, SLOBudget{SLO: "lat", Remaining: f(-1.06)}), BudgetExhausted, VerdictFixOnly, "lat"},
		{"exhausted beats unknown", budgetOf(SLOBudget{SLO: "a"}, SLOBudget{SLO: "b", Remaining: f(-0.1)}), BudgetExhausted, VerdictFixOnly, "b"},
		{"fast burn with budget left", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.95), FastBurn: true}), BudgetFastBurn, VerdictFixOnly, "a"},
		{"an SLO without data", budgetOf(SLOBudget{SLO: "a", Remaining: f(0.9)}, SLOBudget{SLO: "b"}), BudgetUnknown, VerdictFixOnly, "a"},
		{"no SLO", budgetOf(), BudgetUnknown, VerdictFixOnly, ""},
		{"Prometheus unreachable", func() *Budget {
			b := &Budget{App: "app", Env: "prod", SLOs: []SLOBudget{{SLO: "a", Remaining: f(1)}}, Error: "connection refused"}
			b.judge(25)
			return b
		}(), BudgetUnknown, VerdictFixOnly, "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.b.Level != c.level || c.b.Verdict != c.verdict || c.b.Worst != c.worst {
				t.Fatalf("level %s verdict %s worst %s (%s), want %s %s %s", c.b.Level, c.b.Verdict, c.b.Worst, c.b.Detail, c.level, c.verdict, c.worst)
			}
			if c.b.Detail == "" {
				t.Error("no detail")
			}
		})
	}
}

func TestBudgetAllows(t *testing.T) {
	ok, low, out := budgetOf(SLOBudget{SLO: "a", Remaining: f(0.5)}), budgetOf(SLOBudget{SLO: "a", Remaining: f(0.1)}), budgetOf(SLOBudget{SLO: "a", Remaining: f(-0.2)})
	for _, c := range []struct {
		b              *Budget
		change, reason string
		want           bool
	}{
		{ok, "", "", true},
		{ok, "feature", "", true},
		{ok, "hotfix", "x", false}, // unknown kinds are refused, whatever the budget
		{low, "feature", "", false},
		{low, "feature", "   ", false},
		{low, "feature", "release for the demo on Monday", true},
		{low, "fix", "", false},
		{out, "feature", "important", false},
		{out, "fix", "", false},
		{out, "fix", "adds the missing index", true},
		{out, "exception", "budget spent by the ISP outage, see postmortem", true},
		{out, "exception", "", false},
	} {
		if got, why := c.b.Allows(c.change, c.reason); got != c.want || (!got && why == "") {
			t.Errorf("%s change=%q reason=%q: %v (%s), want %v", c.b.Level, c.change, c.reason, got, why, c.want)
		}
	}
}

// fakeProm answers instant queries by the metric name at the start of the query; it records the queries.
func fakeProm(t *testing.T, answers map[string]string) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		seen = append(seen, q)
		for prefix, body := range answers {
			if strings.HasPrefix(q, prefix) {
				w.Write([]byte(body))
				return
			}
		}
		t.Errorf("unexpected query %s", q)
		w.WriteHeader(400)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func vec(samples ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
}

func sample(labels, value string) string {
	return `{"metric":{` + labels + `},"value":[1795206312.8,"` + value + `"]}`
}

// The recorded shape of Pyrra's generic rules on the laptop (2026-11-21): api-factory prod rpc-latency 97.94% of 99%.
func TestFetchBudgets(t *testing.T) {
	srv, seen := fakeProm(t, map[string]string{
		"pyrra_objective": vec(
			sample(`"app":"api-factory","env":"prod","slo":"api-factory-rpc-latency"`, "0.99"),
			sample(`"app":"api-factory","env":"prod","slo":"api-factory-rpc-availability"`, "0.9990000000000001"),
			sample(`"app":"sample-app","env":"prod","slo":"sample-app-hello-probe"`, "0.995"),
			sample(`"app":"storefront","env":"prod","slo":"storefront-web-probe"`, "0.995"),
			sample(`"app":"storefront","env":"prod","slo":"storefront-order-intake"`, "0.995")),
		"pyrra_availability": vec(
			sample(`"app":"api-factory","env":"prod","slo":"api-factory-rpc-latency"`, "0.9793672637445805"),
			sample(`"app":"api-factory","env":"prod","slo":"api-factory-rpc-availability"`, "1"),
			sample(`"app":"sample-app","env":"prod","slo":"sample-app-hello-probe"`, "0.999"),
			sample(`"app":"storefront","env":"prod","slo":"storefront-web-probe"`, "1"),
			sample(`"app":"storefront","env":"prod","slo":"storefront-order-intake"`, "NaN")), // no events in the window
		"ALERTS": vec(sample(`"alertname":"ErrorBudgetBurn","namespace":"sample-app-prod","slo":"sample-app-hello-probe","long":"1h"`, "1")),
	})
	cfg := &ErrorBudget{Prometheus: srv.URL}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m, err := fetchBudgets(srv.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if q := (*seen)[0]; q != `pyrra_objective{env=~"prod"}` {
		t.Errorf("query %s", q)
	}
	af := m["api-factory/prod"]
	if af == nil || af.Level != BudgetExhausted || af.Worst != "api-factory-rpc-latency" || pct(*af.Remaining) != "-106.3%" {
		t.Fatalf("api-factory %+v", af)
	}
	if s := m["sample-app/prod"]; s.Level != BudgetFastBurn || !s.SLOs[0].FastBurn || pct(*s.Remaining) != "80%" {
		t.Errorf("sample-app %+v", s)
	}
	if s := m["storefront/prod"]; s.Level != BudgetUnknown || !strings.Contains(s.Detail, "storefront-order-intake") {
		t.Errorf("storefront %+v", s)
	}
}

func TestBoardBudgetFailsClosed(t *testing.T) {
	cfg, err := ParseConfig([]byte(testConfig + "errorBudget: { prometheus: http://127.0.0.1:1 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Board{cfg: cfg, now: func() time.Time { return t0 }, states: map[string]*PipelineState{},
		prom: &http.Client{Timeout: time.Second}}
	if got := b.budget("app", "prod"); got.Level != BudgetUnknown || !strings.Contains(got.Detail, "还没有查询过") {
		t.Errorf("before the first query: %+v", got)
	}
	b.refreshBudgets() // nothing listens on port 1
	got := b.budget("app", "prod")
	if got.Level != BudgetUnknown || got.Verdict != VerdictFixOnly || !strings.Contains(got.Detail, "Prometheus") {
		t.Errorf("unreachable: %+v", got)
	}
	if b.budget("app", "dev") != nil {
		t.Error("dev is not gated")
	}
	// a known app without any SLO in prod
	b.budgets, b.budgetErr = map[string]*Budget{"other/prod": budgetOf(SLOBudget{SLO: "x", Remaining: f(1)})}, ""
	if got := b.budget("app", "prod"); got.Level != BudgetUnknown || !strings.Contains(got.Detail, "没有 SLO") {
		t.Errorf("no SLO: %+v", got)
	}
}

func fixed(b *Budget) func(app, env string) *Budget {
	return func(app, env string) *Budget {
		if env != "prod" || app != "app" {
			return nil
		}
		cp := *b
		return &cp
	}
}

func TestGateWithBudget(t *testing.T) {
	// enough budget: the gate passes as before, with one more check
	g := collectBudget(t, newWorld(time.Hour), fixed(budgetOf(SLOBudget{SLO: "a", Remaining: f(0.8)}))).Gate("prod")
	if !g.Ready || g.Overridable || len(g.Checks) != 5 || g.Checks[4].Name != "错误预算" || !g.Checks[4].OK {
		t.Fatalf("gate %+v", g)
	}
	if ok, _ := g.Allows("", ""); !ok {
		t.Error("a feature without a reason must pass with 80% left")
	}

	// low: only the budget fails; the promote step says what it needs
	ps := collectBudget(t, newWorld(time.Hour), fixed(budgetOf(SLOBudget{SLO: "a", Remaining: f(0.2)})))
	g = ps.Gate("prod")
	if g.Ready || !g.Overridable || g.failing() != "错误预算" || g.Need() != "需要理由" {
		t.Fatalf("low: ready %v overridable %v failing %q", g.Ready, g.Overridable, g.failing())
	}
	if n := runByID(t, ps, "pr-1").Steps[7].Note; n != "可以 promote(需要理由)" {
		t.Errorf("promote note %q", n)
	}
	if ok, _ := g.Allows("feature", ""); ok {
		t.Error("low budget without a reason passed")
	}
	if ok, _ := g.Allows("feature", "customer demo"); !ok {
		t.Error("low budget with a reason refused")
	}

	// exhausted and still soaking: the budget cannot be overridden while another check fails
	g = collectBudget(t, newWorld(3*time.Minute), fixed(budgetOf(SLOBudget{SLO: "a", Remaining: f(-0.5)}))).Gate("prod")
	if g.Ready || g.Overridable {
		t.Fatalf("exhausted + soak: %+v", g)
	}
	if ok, why := g.Allows("fix", "the fix"); ok || !strings.Contains(why, "在 dev 运行") {
		t.Errorf("a fix skipped the soak: %v %s", ok, why)
	}
}

// A promotion PR opened from GitHub (not the board) declares its change kind and reason in the body; the board
// judges it for the person who merges.
func TestOpenPromotionPRJudged(t *testing.T) {
	for _, c := range []struct {
		body, change, reason, policy string
	}{
		{"Promote the version …\n\nChange: feature\n", "feature", "", "只允许修复"},
		{"Change: fix\nReason: adds the missing index  \n\n```diff", "fix", "adds the missing index", ""},
		{"Change: exception\nReason:\n", "exception", "", "需要写明理由"},
		{"an old PR without the lines", "feature", "", "只允许修复"},
	} {
		w := newWorld(time.Hour)
		w.gh["repos/o/m/pulls?state=open&per_page=30"] = []any{map[string]any{"number": 9, "title": "promote app → prod", "html_url": "p/9", "state": "open", "body": c.body}}
		g := collectBudget(t, w, fixed(budgetOf(SLOBudget{SLO: "a", Remaining: f(-0.1)}))).Gate("prod")
		if g.OpenPRChange != c.change || g.OpenPRReason != c.reason || !strings.Contains(g.OpenPRPolicy, c.policy) || (c.policy == "") != (g.OpenPRPolicy == "") {
			t.Errorf("%q: change %q reason %q policy %q", c.body, g.OpenPRChange, g.OpenPRReason, g.OpenPRPolicy)
		}
	}
}

func TestErrorBudgetConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(testConfig + "errorBudget: { prometheus: http://prom:9090 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	eb := cfg.ErrorBudget
	if strings.Join(eb.Environments, ",") != "prod" || eb.ReasonBelow != 25 || eb.Inputs.Change != "change" || eb.Inputs.Reason != "reason" {
		t.Errorf("defaults %+v", eb)
	}
	if cfg.Pipelines[0].App != "app" {
		t.Errorf("app defaults to the id: %q", cfg.Pipelines[0].App)
	}
	if _, err := ParseConfig([]byte(testConfig + "errorBudget: { environments: [prod] }\n")); err == nil || !strings.Contains(err.Error(), "prometheus required") {
		t.Errorf("missing prometheus: %v", err)
	}
	// discovered services keep the application's name for their SLOs
	raw, _ := os.ReadFile("testdata/storefront-app.yaml")
	ps, err := pipelinesOf(raw, "storefront", testDiscover)
	if err != nil || ps[0].ID != "storefront-web" || ps[0].App != "storefront" {
		t.Errorf("discovered app: %v %+v", err, ps)
	}
}

func TestBudgetPagesAndDryRun(t *testing.T) {
	cfg, _ := ParseConfig([]byte(testConfig + "errorBudget: { prometheus: http://prom:9090 }\n"))
	b := &Board{cfg: cfg, now: func() time.Time { return t0 }, states: map[string]*PipelineState{}, promoted: map[string]time.Time{},
		live: map[string]liveResult{}}
	b.states["app"] = collectBudget(t, newWorld(time.Hour), fixed(budgetOf(SLOBudget{SLO: "app-latency", Remaining: f(-1.063)}, SLOBudget{SLO: "app-probe", Remaining: f(1)})))
	h := b.Handler()
	for path, want := range map[string]string{
		"/":        "预算 -106.3% · 只允许修复",
		"/?p=app":  "app-latency <b>-106.3%</b>",
		"/metrics": `delivery_promotion_budget_verdict{pipeline="app",env="prod",level="exhausted"} 2`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, rec.Code, want)
		}
	}
	for q, want := range map[string]bool{"": false, "?change=feature&reason=x": false, "?change=fix": false, "?change=fix&reason=adds+the+index": true} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/gate/app/prod"+q, nil))
		var r struct {
			Allowed       bool
			Verdict       string
			BudgetVerdict string
			Why           string
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || r.Allowed != want || r.Verdict != VerdictFixOnly || r.BudgetVerdict != VerdictFixOnly {
			t.Errorf("dry run %q: %v %+v", q, err, r)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/gate/app/dev", nil))
	if rec.Code != 404 {
		t.Errorf("dev has no gate: %d", rec.Code)
	}
}
