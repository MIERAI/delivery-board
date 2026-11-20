# Error budget gate

The promotion gate enforces the error budget policy of gitops-platform-blueprint
[docs/slo.md §6](https://github.com/MIERAI/gitops-platform-blueprint/blob/main/docs/slo.md#6-error-budget-policy) for
promotions into the gated environments (`prod`). Code: `budget.go`; tests: `budget_test.go`.

| Budget left (28d) | Gate verdict | A promotion passes when |
|---|---|---|
| > 25% | `pass` | always (the other checks still apply) |
| 0–25% | `needs-reason` | it states a reason |
| ≤ 0 (exhausted) | `fix-only` | it is `change: fix` (a fix or reliability work; a rollback is one) or `change: exception`, **and** states a reason |
| fast burn now | `fix-only` | the same: roll back first (a rollback is `change: fix`) |
| unknown | `fix-only` | the same (fail closed, see below) |

The error budget is one more check, `错误预算`, next to "new version", "healthy", "soak" and "no open promotion". It
never overrides those: a fix still has to soak. When the budget is the only failing check the gate is *overridable*:
the board offers the Promote form with a change kind and a reason, and says what is needed ("需要理由" / "只允许修复").

## Where the numbers come from

Pyrra (laptop, `pyrra kubernetes --generic-rules`) records for every SLO rendered from an `app.yaml` (`platform/slo`
in gitops-manifests, labels `pyrra.dev/app` and `pyrra.dev/env`):

- `pyrra_objective{app, env, slo}`: the target (0.995);
- `pyrra_availability{app, env, slo}`: the good share over the SLO's window (28d), from the `*:increase4w` rules;
- `ALERTS{alertname="ErrorBudgetBurn", long="1h", namespace="<app>-<env>"}`: the fast-burn tier (14× over 1h and
  5m), the one that pages.

Budget left per SLO = `(availability − objective) / (1 − objective)`: 1 untouched, 0 spent, negative overspent (what
Pyrra's UI shows). The app's budget is the **lowest** of its SLOs in the target environment: any SLO of the
application counts, so storefront web, api and worker share storefront's budget (§6 is about the application's
users, and the services are released together into one product). Three queries per refresh for all apps; a failed
query discards the last numbers (an old number must not pass the gate).

Which app: `pipeline.app` (default the pipeline id); discovered pipelines take the `name` of their `app.yaml`.

## Decisions

**How a reason is stated.** A free-text `reason` next to the change kind, on both paths a promotion takes:

- the board's Promote button (with `PROMOTE_TOKEN`): a form with *change* and *reason*, checked by the gate on fresh
  data before the workflow is dispatched, and passed to it as the inputs `change` and `reason`
  (`errorBudget.inputs`);
- GitHub → Actions → promote → *Run workflow* (today's path: the board has no `PROMOTE_TOKEN`): the same two inputs of
  gitops-manifests `promote.yml`.

Either way the workflow writes them into the promotion PR body:

```
Change: fix
Reason: rpc-latency budget spent by the missing index; this adds it
```

so the person who merges sees them, and the board reads them back from the open PR and shows whether it satisfies
the policy (`#N 声明 … 不符合预算政策: …` on the gate, `OpenPRPolicy` in `/api/state`). Workflow inputs rather than PR
labels: the promotion is started by a dispatch with inputs already (version, soak), labels would have to exist in the
repository and can be added after the fact without a trace in the run.

**How a fix is marked.** `change: feature | fix | exception`, default `feature`. `fix` covers fixes, reliability work
and rollbacks; `exception` is §6's explicit exception (budget spent by a dependency outside our control, a bad SLI)
and belongs in the postmortem. Both need a reason, so that every promotion past an exhausted budget leaves a sentence
in its PR. The workflow refuses `fix` / `exception` without a reason on its own (it cannot see the budget).

**Unknown budget: fail closed** (treated like an exhausted budget: fixes and explicit exceptions only). Unknown means:
Prometheus unreachable or a query failed, the app has no SLO in the environment, or one of its SLOs has no data
(`pyrra_availability` absent or NaN: no events in the window, a renamed metric). Why closed:

- §2 "no data is not zero errors": a missing SLI must not read as a healthy one, and an unknown budget is exactly
  that;
- the gate is the only place the policy is enforced; failing open would make it vanish precisely when monitoring is
  broken, which is when a bad promotion is least visible;
- it does not deadlock delivery: a fix ships with `change: fix` and a reason, and a monitoring outage or a bad SLI is
  an exception §6 already names (`change: exception` + reason), decided explicitly instead of silently.

An app without any SLO therefore needs `change: exception` for prod: every user-facing service has one (§2).

**Exhausted beats unknown.** An app with one SLO overspent and another without data is `exhausted`, not `unknown`
(the detail names the worst known SLO).

## Not enforced here

- **Merging is not blocked.** The board only reads GitHub (fine-grained, read-only token) and GitHub's runners cannot
  reach the laptop's Prometheus, so neither can set a required check. The workflow records the declaration, the board
  judges the open PR, a person merges. A required status (`error-budget`) from the board would need `statuses: write`
  on gitops-manifests: a follow-up (ROADMAP).
- **"Roll back first" is advice.** During a fast burn the gate allows only fixes (a rollback is one) and says so; it
  does not roll back.
- **Young SLOs.** The window is 28 days but an SLO has only the history it has: a fresh SLO's budget moves fast
  (§4, "a young SLO is oversensitive"). The gate takes the number as Pyrra shows it.

## API

- `/api/state`: every gate has `Budget` (`Level`, `Verdict`, `Remaining`, `Worst`, `Detail`, per-SLO `SLOs`),
  `Overridable`, and for an open promotion PR `OpenPRChange`, `OpenPRReason`, `OpenPRPolicy`.
- `/api/gate/<pipeline>/<env>?change=fix&reason=…`: **dry run** of a promotion on the board's current state; nothing
  is dispatched. `verdict` (the whole gate: `pass`, `needs-reason`, `fix-only`, or `blocked` when another check
  fails), `budgetVerdict` (the budget alone), `allowed`, `why`.
- `/metrics`: `delivery_error_budget_remaining_ratio{pipeline, env}` (the lowest known), and
  `delivery_promotion_budget_verdict{pipeline, env, level}` (0 pass, 1 needs a reason, 2 fixes and exceptions only).

## Configuration

```yaml
errorBudget:
  prometheus: http://kps-prometheus.monitoring.svc:9090
  environments: [prod]          # default [prod]
  reasonBelow: 25               # percent; default 25
  inputs: { change: change, reason: reason }   # the promotion workflow's inputs; the defaults
```

Without `errorBudget` the gate is as before. The promotion workflow of a gated environment must accept the two inputs.
