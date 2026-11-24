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
- **Error budget.** Promotions into prod also check the app's SLO error budget (Pyrra, 28d): over 25% left passes,
  0–25% needs a stated reason, exhausted / fast burn / unknown lets only fixes and explicit exceptions through
  ([docs/error-budget.md](docs/error-budget.md)).
- **One program, any pipeline.** Pipelines are configuration ([examples/config.yaml](examples/config.yaml)): which PRs
  start a run, which workflows and jobs are its steps, which manifests file and workload are an environment.

Pages: `/` (gates + history of every pipeline), `/run/<pipeline>/<id>` (the run as a flow graph), `/api/state` (JSON),
`/api/gate/<pipeline>/<env>?change=&reason=` (dry run of a promotion),
`/metrics` (Prometheus: run status, promotion ready, commits behind, DORA metrics: [docs/dora.md](docs/dora.md)),
`/healthz`.

Run states: 成功 done, 进行中 running, 等人处理 waiting for a person (review answer, merge, promote), 失败 failed,
未开始 waiting, 无法观测 unknown (git delivered the version to an environment the board cannot see), 已被取代
superseded: a newer run of the pipeline (a later commit, so it includes this one) has reached or waits at the same
delivery step, so delivering it delivers this one too. A superseded run is dimmed, says "已包含在 <sha>" and is not
counted as waiting; only the newest run keeps the promote. What is done stays done.

## Model

| Config | Source | Meaning |
|---|---|---|
| `changes[]` | pull requests (head branch / title regexp) | a run per matching PR; `items` names it from the changed files |
| `steps[].workflow` | workflow runs | before `merge`: runs on the PR branch (rounds); after: the run on the current commit (`jobs`: job → step) |
| `steps[].merge` | the PR | merged → the current commit is its merge commit |
| `steps[].followPR` | pull requests | a PR opened by automation for the current commit (`$(sha7)`); its merge commit becomes current |
| `push` | release runs on the branch | commits that no change covers are runs too |
| `release` | workflow run on the commit | build and deploy to the first environment |
| `environments[].manifests` | commits of a file in the manifests repo | the desired version (image tag) and when it arrived: a kustomization (`images[name].newTag`) or Helm values (the mapping `{repository: <image>, tag}`, or `key: a.b.tag`) |
| `environments[].argocd`, `.workload` | the cluster | sync / health, the running version, rollout finished |
| `environments[].unobserved` | — | the environment's cluster is not the board's (e.g. a lab): once git has a version it shows as 无法观测 (unknown), never as running. Also automatic while neither the workload nor the Argo CD Application can be read. `unobserved: true` on an `app.yaml` environment too |
| `environments[].promotion` | — | `from`, `soak`, the workflow to dispatch (`inputs`, `versionInput`, `soakInput`), the title of its PRs |
| `errorBudget` | Prometheus (Pyrra rules) | gate promotions into these environments on the error budget of the pipeline's `app` ([docs/error-budget.md](docs/error-budget.md)) |

### Discovery

Applications registered by an `app.yaml` in a manifests repository need no configuration here: `discover` turns each
service of each `*/app.yaml` into a pipeline (format: gitops-manifests `storefront/app.yaml`; refreshed every 2 minutes;
a descriptor that does not parse is shown and skipped).

```yaml
discover:
  - repo: MIERAI/gitops-manifests
    jobs: { test: test, "image / build": 镜像 }       # release workflow (release-<service>): job → step
    promotion: { workflow: promote.yml, soakInput: soak }   # inputs app, image, from, to, version
```

### Templates

Many pipelines look alike (the services of a monorepo, environments that differ only by name). Two keys and three
placeholders expand the configuration before it is validated; `-print-config` prints the result.

```yaml
  - id: storefront
    repo: MIERAI/storefront
    services: [web, api]                     # → pipelines storefront-web, storefront-api; $(service) = the service
    release: { name: release-$(service) }
    defaults:
      environment:                           # merged under every environment; $(env) = its name
        manifests: { repo: MIERAI/gitops-manifests, path: storefront/overlays/$(env)/kustomization.yaml, image: ghcr.io/mierai/storefront-$(service) }
        argocd: { app: storefront-$(env) }
        workload: { namespace: storefront-$(env), name: $(service), container: $(service) }
      promotion:                             # merged under every promotion; $(from) = its from
        prTitle: ^promote storefront storefront-$(service) → $(env)$
        workflow: { file: promote.yml, inputs: { app: storefront, image: ghcr.io/mierai/storefront-$(service), from: $(from), to: $(env) } }
    environments:
      - name: dev
      - { name: staging, promotion: { from: dev, soak: 5m } }
      - { name: prod, promotion: { from: staging, soak: 10m } }
```

An environment's own keys win over the defaults (mappings merge key by key). A placeholder outside its scope is an
error. `followPR` has its own: `$(sha7)`, the current commit.

A commit is *in* an environment when the environment's version is that commit or a later one of the branch.
"Behind" counts the pipeline's own releases (in a monorepo, other services' commits do not count).

Validated shapes ([examples/config.yaml](examples/config.yaml)): code generated from design PRs (review bot, follow-up
PR), a plain push pipeline with canary Rollouts, and a monorepo whose services are released (path filters) and promoted
(one image) on their own through three environments, deployed with kustomize (shared overlays) and with a Helm chart.

## Run

```sh
go run . -config examples/config.yaml -check            # validate
go run . -config examples/config.yaml -print-config     # with services / defaults expanded
K8S_API=http://127.0.0.1:8001 GITHUB_TOKEN=… go run . -config examples/config.yaml -addr :8080   # kubectl proxy
```

| Env | |
|---|---|
| `GITHUB_TOKEN` | fine-grained, read-only: Actions, Contents, Pull requests (metadata) of the app and manifests repositories |
| `PROMOTE_TOKEN` | optional, Actions read/write on the repository of the promotion workflow; without it the button links to the workflow |
| `K8S_API` | local development only; in the cluster the service account is used (RBAC: read-only `get` on workloads and Argo CD Applications, cluster-wide so new applications need nothing) |

The board has no login: run it on a private network. Promoting only opens a PR; merging stays on GitHub.

## Roadmap

What is parked and in which order: [ROADMAP.md](ROADMAP.md).

## Development

`go test ./...` covers the run model against recorded API shapes (design change → gate, soak, health, open PR,
review failure, rollback, syncing) and renders every page.
