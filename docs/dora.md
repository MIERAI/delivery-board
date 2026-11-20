# DORA metrics

The board measures the four DORA metrics per pipeline and environment from what it reads anyway: the history of the
environment's manifests file (one commit per deployment, promotion or rollback) and the recent commits of the
application's branch. **No extra GitHub calls**: the branch commits already carry their message and date, the
manifests history is already read for the arrival times. Code: `dora.go`; tests: `dora_test.go`.

Production is the environment named `prod`; the metrics exist for every environment (dev's rollbacks are visible too).

## Definitions

| Metric | Definition | From |
|---|---|---|
| **Deployment** | the environment's image tag changes in its manifests file. Consecutive commits with the same tag are one version; the oldest version of the history is the baseline, not a deployment. A rollback is a deployment too. Time = committer date of the manifests commit (the merge of the promotion PR). | manifests history |
| **Deployment frequency** | deployments per day. | count of deployments |
| **Lead time for changes** | committer date of the deployed version's commit on the branch (the merge to main) → its arrival in the environment. Per deployment; rollbacks excluded. | branch commits + manifests history |
| **Rollback** | a deployment of a version **older** than the one it replaces. Older = further down the branch (the tag is a commit of the branch); when the branch does not know a tag (semver, older than the last 100 commits), the order of first arrival in the environment's history decides. | both |
| **Change failure** | a deployment D failed when a later deployment restores what preceded it: (a) a rollback to a version not newer than D's predecessor — the *first* of the newer deployments it undoes failed; or (b) a forward deployment brings a revert commit (`Revert …` with git's `This reverts commit <sha>` line) of a commit that D brought. | both |
| **Change failure rate** | failed deployments / deployments, by the deployment's time. | |
| **Time to restore** (failed deployment recovery time) | from the failed deployment to the deployment that restored it (the rollback or the revert). | manifests history |

Example (sample-app dev, 2026-11-18): `bb5ecc1` arrived 00:03:18Z, `Rollback sample-app dev to f548864` 00:03:58Z →
one change failure, restored in 40 s; the roll forward to `bb5ecc1` again is a normal deployment.

## Metrics

Labels: `pipeline`, `env` (as the board's other metrics). Lead and restore time buckets (seconds): 900, 3600, 14400,
**86400** (the lead time SLO threshold), 259200, 604800.

Two kinds, because the board keeps no state and Prometheus keeps 3 days (`retention: 3d`) while the SLO window is 28:

| Metric | Type | Meaning |
|---|---|---|
| `delivery_deployments_total` | counter | deployments this process saw happen |
| `delivery_change_failures_total` | counter | failures this process detected (when the restoring deployment arrives) |
| `delivery_lead_time_seconds` | histogram | lead time of those deployments |
| `delivery_restore_time_seconds` | histogram | restore time of those failures |
| `delivery_window_deployments` | gauge | deployments of the last 28 days, recomputed from the history at every scrape |
| `delivery_window_change_failures` | gauge | of those, the failed ones |
| `delivery_window_lead_time_seconds_{bucket,sum,count}` | gauges | histogram-shaped (cumulative `le` buckets): `histogram_quantile` works on them |
| `delivery_window_restore_time_seconds_{bucket,sum,count}` | gauges | the same for restore time |
| `delivery_window_covered_seconds` | gauge | how much of the 28 days the history covers (< 2419200: deployments before it are not seen) |

**Restarts.** The counters start at 0: the first successful refresh of a pipeline is a baseline that is not counted,
so a restart is an ordinary counter reset for Prometheus (`rate`/`increase` stay right) instead of replaying the
history as new events. Events while the board is down are missed by the counters. The `delivery_window_*` gauges are
recomputed from git at every refresh: a restart changes nothing, a deployment while the board was down is there. **The
SLOs use the window gauges.** The counters are for timelines over hours or days.

## Blind spots

- **Only image tag changes are deployments.** A configuration change (an environment variable, replicas, a fault switch
  such as `FAULT_HELLO`) and its revert are neither deployments nor failures.
- **Git time, not cluster time.** A deployment is the merge in the manifests repository; Argo CD syncs seconds to
  minutes later. A sync that fails is not a deployment the board can tell apart; neither is a canary that Argo
  Rollouts aborts while git keeps the new version (that failure is visible only if someone then rolls git back or
  reverts the commit).
- **A failure is only known once it is restored**, and only when restored by a rollback or a `git revert`. A fix
  forward (a new commit that repairs the bug) is a normal deployment: the failure is not counted. A failure that is
  still open is not counted yet, so the failure rate of the last days is optimistic.
- **Lead time is per deployment, from its newest commit**: a lower bound. Older commits batched into the same
  deployment waited longer. It starts at the merge to main, not at the first commit or the PR. In a monorepo, the
  deployed commit can be another service's (the release builds from the head of main).
- **Revert attribution needs git's message** (`This reverts commit <sha>`). A squash-merged revert PR keeps it only if
  the squash message includes the commit body; a revert without it is not attributed.
- **History depth.** 40 commits of the manifests file, 100 commits of the branch (ROADMAP 7). A busy environment's
  history may not reach 28 days back: `delivery_window_covered_seconds` says so, the window ratios are then over the
  part covered. A tag older than the last 100 branch commits has no lead time and is ordered by first arrival.
  A moved manifests file restarts the history.
- **Production incidents not caused by a deployment** are not seen at all: that is what the availability and latency
  SLOs of the applications are for. Time to restore here is "failed deployment recovery time" only.
- **Small numbers.** A few production deployments a week make every ratio jumpy (1 of 4 = 25 %). The SLO rules
  require a minimum number of deployments before a ratio can open a ticket.

## SLOs

Declared in gitops-manifests `platform/delivery-board/slo.yaml` (PrometheusRule, `severity: ticket`; dashboard
"Delivery (DORA)"), over the 28-day window gauges, production only:

| SLI | Objective | Alert |
|---|---|---|
| lead time ≤ 1 day | 90 % of prod deployments | ticket when the slow ones exceed 10 % of max(deployments, 10) |
| change failure | ≤ 15 % of prod deployments fail | ticket when failures exceed 15 % of max(deployments, 10) |
| deployment frequency | none (dashboard) | — |
| time to restore | none (dashboard; each failure is reviewed on its own) | — |

Never pages (blueprint `docs/slo.md` §4): no user is affected *now* by a slow or failing delivery process, nothing
must be done tonight, and the signal moves over days. Not Pyrra: its ratio objectives need counters over the window,
and Prometheus keeps 3 days; with a handful of events a burn rate is meaningless.
