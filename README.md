# delivery-board

Every change of an application as one run — from its pull request to the last environment — and a gate for promoting
between environments. Like a CI build history, but the build is the whole delivery.

```
design PR ─ AI review ─ merge ─ generate ─ generated PR ─ release ─ dev ─ ⏸ promote (gate) ─ prod
   push ──────────────────────────────────────────────── release ─ dev ─ ⏸ promote (gate) ─ prod
```

- **Starts before the image exists.** Pull requests and the automation after them (review bots, code generation,
  follow-up PRs) are steps of the same run as the deployments. Tools that start at the image registry cannot show them.
- **No state of its own.** Every refresh derives everything from GitHub (PRs, workflow runs, the history of the
  manifests repository) and the cluster (Argo CD, the running workloads). Restart it, run two, delete it: nothing is
  lost and it never disagrees with git. No CRDs, no database.
- **Promotion gate.** For each promotion: is there a newer version, is the source environment healthy and running it,
  has it run there long enough (soak), is no promotion already open. The *Promote* button re-checks the gate and starts
  your promotion workflow; a person still merges the PR it opens.
- **One program, any pipeline.** Pipelines are configuration ([examples/config.yaml](examples/config.yaml)): which PRs
  start a run, which workflows and jobs are its steps, which manifests file and workload are an environment.

Pages: `/` (gates + history of every pipeline), `/run/<pipeline>/<id>` (the run as a flow graph), `/api/state` (JSON),
`/metrics` (Prometheus: run status, promotion ready, commits behind), `/healthz`.

## Model

| Config | Source | Meaning |
|---|---|---|
| `changes[]` | pull requests (head branch / title regexp) | a run per matching PR; `items` names it from the changed files |
| `steps[].workflow` | workflow runs | before `merge`: runs on the PR branch (rounds); after: the run on the current commit (`jobs`: job → step) |
| `steps[].merge` | the PR | merged → the current commit is its merge commit |
| `steps[].followPR` | pull requests | a PR opened by automation for the current commit (`{sha7}`); its merge commit becomes current |
| `push` | release runs on the branch | commits that no change covers are runs too |
| `release` | workflow run on the commit | build and deploy to the first environment |
| `environments[].manifests` | commits of a file in the manifests repo | the desired version (image tag) and when it arrived |
| `environments[].argocd`, `.workload` | the cluster | sync / health, the running version, rollout finished |
| `environments[].promotion` | — | `from`, `soak`, the workflow to dispatch (`inputs`, `versionInput`, `soakInput`), the title of its PRs |

A commit is *in* an environment when the environment's version is that commit or a later one of the branch.
"Behind" counts the pipeline's own releases (in a monorepo, other services' commits do not count).

Validated shapes ([examples/config.yaml](examples/config.yaml)): code generated from design PRs (review bot, follow-up
PR), a plain push pipeline with canary Rollouts, and a monorepo whose services share manifests files but are released
(path filters) and promoted (one image) on their own through three environments.

## Run

```sh
go run . -config examples/config.yaml -check            # validate
K8S_API=http://127.0.0.1:8001 GITHUB_TOKEN=… go run . -config examples/config.yaml -addr :8080   # kubectl proxy
```

| Env | |
|---|---|
| `GITHUB_TOKEN` | fine-grained, read-only: Actions, Contents, Pull requests (metadata) of the app and manifests repositories |
| `PROMOTE_TOKEN` | optional, Actions read/write on the repository of the promotion workflow; without it the button links to the workflow |
| `K8S_API` | local development only; in the cluster the service account is used (RBAC: `get` on the workloads and Argo CD Applications) |

The board has no login: run it on a private network. Promoting only opens a PR; merging stays on GitHub.

## Development

`go test ./...` covers the run model against recorded API shapes (design change → gate, soak, health, open PR,
review failure, rollback, syncing) and renders every page.
