# Roadmap / memo (2026-11-20)

Where the board stands and what to do next, in order. Context: started as `board.py` for api-factory only; became
config-driven and validated on three shapes (design-PR driven, push, monorepo with kustomize + Helm), five pipelines,
two- and three-stage environments. Kargo was evaluated and not adopted (it cannot see the steps before the image, and
it keeps its own state that drifts from git).

Assessment: platform component ~80, internal multi-team ~65, open source ~50. The idea holds; two structural limits
(1, 2) block most outside teams and get harder to change later.

## Next, structural (do first)

1. **Version model: tag ≠ commit.** "Commit X is in environment E" assumes the image tag is a commit of the branch
   (`contains`, `behind`, push runs). Semver tags (`v1.2.3`) do not work. Read the commit from the image instead
   (OCI annotation `org.opencontainers.image.revision`, set by build-image), keep tag-as-commit as the fast path.
2. **GitHub API budget.** Done: conditional requests (a 304 is free), one request per path and refresh, a reserve and
   back-off on the rate limit (README "API budget": 12 pipelines at 30 s went from ~8,800 to ~0–360 counted requests an
   hour). Left: webhooks to refresh on change instead of polling (fewer requests sent, fresher pages).
3. **Authentication.** No login; the Promote button relies on a private network. oauth2-proxy in front (GitHub org /
   team), then pass the user to the dispatch (audit: who promoted).

## Then, features (parked, each small)

4. **Metrics gate.** The gate checks health and soak, not quality: sample-app `1f67fe5` (FAULT_HELLO=contention, a load
   regression) shows "可以 promote". Add Prometheus checks per promotion (error ratio, p95 of the version in `from`);
   later read the SLO error budget (the SLO project).
   DORA metrics and two delivery SLOs exist (docs/dora.md). The error budget policy (blueprint docs/slo.md §6) is
   in the gate (docs/error-budget.md). Left: a required `error-budget` status on the promotion PR (needs
   `statuses: write`), so a PR opened from GitHub that ignores the policy cannot be merged, not only shown.
5. **Why a gate is blocked, per service.** web and api share the Argo app `storefront-dev`: api broken → web's gate
   fails on "dev 健康". Judge health by the service's own resources (Argo `status.resources`), list the unhealthy ones.
6. **Per-service overrides in `services` templates.** storefront-worker (Helm) cannot join `services: [web, api]`
   (different paths / app names), so it is a separate ~20-line pipeline.
7. **History depth, renames.** The manifests history (GitHub commits?path=) does not follow a moved file: after a move the arrival time restarts (soak counted again once).
   **History depth.** Last 100 branch commits, 100 runs, 40 manifests commits: an environment far behind is misjudged.
   Page further when a tag is not found, or use the revision from (1).
8. **Promote token.** The button only links to GitHub until `delivery_board.promote_token` is set (fine-grained:
   Actions read/write on gitops-manifests; SOPS → tofu apply).

## Small / known

- First seconds after a restart: header shows "环境 0/0 健康" instead of "收集中".
- Many pipelines: the tab row wraps; needs search or grouping by team.
- UI text is Chinese, hard-coded: move to a message table before i18n.
- "Try it" is api-factory specific but lives in the core: make it a plugin.
- `images.py` (gitops-manifests) does not handle `image: repo:tag` single strings in Helm values.
- New application: Argo syncs before the first image exists and the signature policy rejects it until a retry;
  build first, then merge the manifests.
- Coverage ~65 %; no end-to-end test against a fake GitHub server.

## Where things are

| What | Where |
|---|---|
| Code, tests, example config | this repository (`examples/config.yaml`, `testdata/handwritten.yaml`) |
| Deployed config + RBAC | gitops-manifests `platform/delivery-board/` (laptop only, `:30090`) |
| Promotion workflow / scripts | gitops-manifests `.github/workflows/promote.yml`, `scripts/{promote,set-image}.sh`, `images.py`, `render.py` |
| Secrets | infrastructure-live SOPS `delivery_board.{github_token,promote_token}` → secret `delivery-board-github` |
| Reference apps | MIERAI/sample-app, MIERAI/storefront (web, api, worker), MIERAI/api-factory (private) |
