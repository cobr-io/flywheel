# Plan: per-app sync controller (git-auto-sync Go port)

**Design:** [../designs/2026-07-17-per-app-sync-controller-design.md](../designs/2026-07-17-per-app-sync-controller-design.md)
**Status:** in progress

Plan organised into sequential phases. Each phase ends in a verifiable,
working state. Do not start a phase until the previous phase is complete and
verified. Tick each task (`- [ ]` → `- [x]`) immediately when complete and
commit it in the same step — do not batch.

Design open questions resolved as the leanings the approved design recorded:
URL prefix filter (Q1), duplicate the ~40-line git runner instead of touching
selfsync (Q2), new nightly-only `scenario-6` (Q3), 30s conflict-stall requeue
(Q4).

## Phase 1: scaffolding — binary and image lanes build

`cmd/git-auto-sync` exists and builds into the existing image lane; no
behavior yet.

- [x] `cmd/git-auto-sync/main.go`: env config (`WORKSPACES_MOUNT`,
      `GIT_SERVER_URL`, `BUILDER_NAMESPACE`, `POLL_INTERVAL`,
      `MAX_CONCURRENT`, `HEALTH_PROBE_ADDR` — defaults per design),
      `syscall.Umask(0)` first, controller-runtime manager with cache scoped
      to `BUILDER_NAMESPACE`, leader election off, scheme = core + apps +
      sourcev1, healthz/readyz via manager checks (image-builder-controller
      idiom).
- [x] `internal/appsync` skeleton: `Ticker`, `TickResult`, `FluxPatcher`
      interface, git runner (duplicated from selfsync per Q2) with per-exec
      context timeout (default 30s).
- [x] `Dockerfile.git-auto-sync`: binary-COPY pattern (issue #46 style like
      the other controllers) — keep `git`, drop `kubectl` and the sync.sh
      COPY. (sync.sh itself is deleted in Phase 4.)
- [x] `Makefile`: move `git-auto-sync` from the script-only bucket into the
      cross-compiled controllers loop (`go build ./cmd/git-auto-sync` into
      the throwaway context).
- [x] `.goreleaser.yaml`: new build id for `cmd/git-auto-sync`; switch both
      `git-auto-sync-{amd64,arm64}` docker ids from
      `extra_files: scripts/git-auto-sync` to the controller binary pattern.
      Follow `docs/dev/add-controller-image.md`; image name and `{{ .Tag }}`
      templating unchanged (release-gotchas memory).

**Verification:** `go build ./... && go vet ./...` green; `make images`
builds all four `flywheel-dev/*` images; `goreleaser release --snapshot
--clean` passes locally.

## Phase 2: appsync core — the race-free tick, unit-proven

`internal/appsync.Ticker.Tick` implements the design's tick end-to-end with
deterministic race tests.

- [x] Snapshot helpers: `symbolic-ref --short HEAD` (branch `B`; detached →
      skip), `for-each-ref` snapshot of all `refs/heads/*`, resolve
      `L = refs/heads/B`. All later decisions take `L`, never `HEAD`.
- [x] Port `heal_index_if_corrupt` (issue #4) and the exit-code-aware dirty
      classification (`diff --quiet` 0/1/>1 semantics).
- [x] Branch-follow: when `trackedBranch != B`, `FluxPatcher` ensures the
      `kustomize.toolkit.fluxcd.io/reconcile: disabled` annotation then
      patches `spec.ref.branch = B` (monotonic; no LAST_BRANCH state).
- [x] Fetch bare `B` (objects only) → remote head `R`; ancestry decisions
      against `L`.
- [x] Integrate path: dirty-guard → re-verify `symbolic-ref == B` →
      `reset --hard R` → **post-verify** (re-read symbolic-ref + diff ref
      snapshot; roll back any moved ref ≠ `refs/heads/B`, abort tick).
- [x] Push path: `push --force-with-lease=B:R <url> L:refs/heads/B`; plain
      push fallback for a brand-new bare branch.
- [x] Divergence path: rebase; on conflict abort + loud log; tick returns a
      stall marker → reconciler requeues at ~30s (Q4 parity).
- [x] Reconcile poke: `reconcile.fluxcd.io/requestedAt` on the GR whenever
      the bare head changed (push or integrated fast-forward) — mirror
      `naming.ReconcileRequestAnnotation`.
- [x] Test-hook seam (`testHook func(stage string)`) at post-snapshot,
      pre-reset, post-reset.
- [x] Unit tests on real on-disk git repos: follow/patch, push-guard idle
      no-op, worktree-ahead push, bare-ahead fast-forward, divergence rebase,
      conflict stall, corrupt-index heal, dirty-guard refusal.
- [x] Race-injection tests: (a) checkout between snapshot and fetch → no-op;
      (b) checkout before reset → re-verify skips integrate; (c) checkout
      after reset (hook moves HEAD) → post-verify rolls back and aborts;
      (d) file modes: root-written files remain writable cross-uid
      (umask assertion — mode check, no second uid needed).

**Verification:** `go test ./internal/appsync/... -count=1 -race` green,
including all four race cases.

## Phase 3: reconciler + wiring

The controller discovers apps from GitRepositories and drives Ticker; binary
is functionally complete.

- [x] `Reconciler`: URL prefix filter (`GIT_SERVER_URL`-based, Q1), worktree
      derivation (URL path basename under `WORKSPACES_MOUNT`), per-app
      Ticker cache, `RequeueAfter = POLL_INTERVAL`,
      `MaxConcurrentReconciles = MAX_CONCURRENT`.
- [x] Legacy interlock: skip + warn-once while Deployment
      `git-auto-sync-<app>` exists in `BUILDER_NAMESPACE`.
- [x] Missing worktree / absent GR → requeue with backoff, no crash loops.
- [x] Fake-client tests: discovery + URL filtering, RequeueAfter value,
      interlock skip + single warn, missing-worktree backoff, stall requeue.
- [x] Wire Reconciler into `cmd/git-auto-sync`; readyz tracks cache sync.

**Verification:** `go build ./... && go vet ./... && go test ./...` green.

## Phase 4: manifests, RBAC, CLI, cleanup

Cluster and CLI ship the controller; the bash sidecar is gone from all
render paths.

- [ ] `manifests/dev-loop/base/git-auto-sync.yaml`: Deployment (image
      `ghcr.io/cobr-io/git-auto-sync:rewritten-by-flywheel-up`, `/workspaces`
      hostPath, probes, resources) + register in the base kustomization.
- [ ] `manifests/dev-loop/base/rbac.yaml`: `gitrepositories` gains
      `list, watch`; new `apps/deployments get, list, watch` rule on the
      flywheel-system `git-auto-sync` Role; update rationale comments.
- [ ] Flip `bootstrapImageOwners["git-auto-sync"]` → `imgOwnerDevLoop` in
      `internal/cli/converge/bootstrap.go`; update the two bootstrap
      kustomization templates' `images:` blocks accordingly; keep
      `TestBootstrapImages_TemplateUnionMatchesSchema` + naming-agreement
      tests green.
- [ ] Confirm the SSA path (up step 11a ApplyDevLoop) rewrites the new
      Deployment's image by name on the dev-loop path — the
      two-apply-paths rule; adjust if the rewrite is list-based.
- [ ] Delete `manifests/per-app-template/git-auto-sync.yaml.tmpl` + its
      entry in `manifests/per-app-template/kustomization.yaml.tmpl`; drop
      `GitAutoSyncImage` plumbing from `internal/cli/add/app`; update
      add-app tests.
- [ ] Delete `scripts/git-auto-sync/` (sync.sh); grep for stragglers
      (Makefile comment, naming-agreement comment about the bash copy,
      uninstall.sh, docs).
- [ ] Update prose: per-app `README.md.tmpl`, client-skeleton
      `AGENTS.md.tmpl`, `builders/README.md.tmpl` — sidecar → controller.

**Verification:** `go test ./...` green; scratch client `flywheel init` +
`up` reaches 5/5 Ready with `git-auto-sync` Deployment Running;
`flywheel add app` renders no sidecar; branch switch in the app worktree
propagates (spec.ref.branch patched, content deploys) per controller logs.

## Phase 5: e2e stress scenario + docs

The acceptance harness exercises the race deliberately; migration is
documented.

- [ ] `testdata/scenarios/scenario-6-branch-stress.sh`: ~10 sub-second app
      branch flips (`feat/stress` ↔ `main`), settle, assert (a) served text
      = final branch's content, (b) bare `main` sha == worktree
      `refs/heads/main` sha (no poison), (c) `index.html` still writable by
      the runner user. Order-coupling header note (requires scenario-1's
      app), lib.sh helpers reused.
- [ ] Append scenario-6 to `run-all.sh` (nightly + local `scripts/e2e.sh`
      inherit it; per-PR stays "1 5") and document it in
      `docs/dev/dev-loop-validation.md`.
- [ ] Migration note: guide entry + release notes — delete
      `builders/base/<app>/git-auto-sync.yaml` from the gitops repo (Flux
      prunes); interlock behavior described.

**Verification:** full local `scripts/e2e.sh` green including scenario-6.

## Phase 6: validation + landing

- [ ] Push branch, open PR (design doc linked, `Closes #86`); per-PR CI
      green.
- [ ] Dispatch `e2e-full` ×3 on the branch; all green with warm-leg latency
      gates unchanged.
- [ ] Post-merge: verify #86 auto-closed; update project memory (port
      shipped; sync.sh gone).

**Verification:** 3 consecutive green `e2e-full` dispatch runs on the
branch; PR merged.

## Re-planning log

- 2026-07-17 — orchestrator review of Phase 2 found the divergence-rebase path
  mutates the worktree without the post-verify guard (design had declared
  integrate the only mutating path); extended the shared post-verify to the
  rebase path + race test (e). User approved.
