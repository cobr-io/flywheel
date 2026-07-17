# Design: per-app sync controller (git-auto-sync Go port)

**Status:** accepted
**Author:** Matthijs van der Kroon (with Claude)
**Date:** 2026-07-17

## Problem

The per-app `git-auto-sync` sidecar (`scripts/git-auto-sync/sync.sh`) has a
TOCTOU race that corrupts the bare repo. The loop samples the checked-out
branch once per iteration; when a `git checkout` in the host worktree lands
mid-iteration, the loop applies feature-branch decisions to a worktree that is
now on `main`:

1. Iteration starts on `feat/x`: `branch=feat/x`, fetch → `FETCH_HEAD` =
   feat sha.
2. The developer (or e2e) checks out `main`.
3. `work_head=$(git rev-parse HEAD)` now reads main's tip; the ancestry test
   concludes "bare is strictly ahead" (feat was cut from main) and runs
   `git reset --hard <feat-sha>` **while checked out on main** — moving
   `refs/heads/main` onto the feature commit and rewriting worktree files as
   root.
4. The next iteration "honestly" pushes `main:main` = feat sha (the lease
   passes; it is even a fast-forward). The bare repo's `main` is now poisoned;
   the artifact reads `main@<feat-sha>`, the freshly-minted higher-`ts` image
   tag wins the ImagePolicy, and worktree and bare agree forever — wedged.

This is the real root cause of the issue #86 nightly failures (scenario 4
revert-leg timeouts), and of run #21's `index.html: Permission denied` (the
root-owned files facet). Full diagnosis with the tag-ledger evidence:
<https://github.com/cobr-io/flywheel/issues/86#issuecomment-5004283813>.
The PR #113 build-controller guard is defense-in-depth at the Flux layer and
cannot see this git-layer corruption (the poisoned artifact's branch label is
honest).

The gitops/self repo is already immune: `internal/selfsync`
(git-deploy-controller) never follows `HEAD`, resolves explicit
`refs/heads/<branch>`, and never mutates the host worktree. The per-app side
cannot simply adopt that model — auto-follow ("switch branches, see them
deploy") and bidirectional sync (a teammate/CI push straight to the bare repo
fast-forwards the worktree) are product behavior — so it needs a race-free
redesign of its own.

**In scope:** replace all per-app bash sidecars with one Go controller;
race-free sync semantics; the root-owned-files fix; legacy-sidecar interlock;
migration docs; unit race tests + a new e2e stress scenario.

**Out of scope:** the gitops/self path (selfsync stays untouched); a
Kustomization poke for app syncs (parity: sync.sh never had one); issue-#6
Tier-2 interval tuning; event-driven (fsnotify) ticking — the 2s poll cadence
is kept as-is.

## Approach

One controller replaces N sidecars (the issue-#6 Tier-3 consolidation),
reconcile-driven so per-app serialization and lifecycle come from the
workqueue instead of hand-managed goroutines.

- **`cmd/git-auto-sync`** — new Go binary in the existing
  `ghcr.io/cobr-io/git-auto-sync` image lane. `Dockerfile.git-auto-sync`
  becomes a Go build (like the other controllers): keeps `git`, drops
  `kubectl` (client-go replaces it). Reusing the image/package name avoids new
  ghcr publish chores and keeps the goreleaser lane shape.
- **`internal/appsync`** — the race-free tick and git helpers. Shares
  selfsync's idioms (explicit refs, push-guard, shell-out runner) but is a
  separate package; selfsync is not modified.
- **Reconciler on per-app GitRepositories** (controller-runtime manager,
  image-builder-controller idiom), namespace-scoped to flywheel-system and
  filtered to GRs whose `spec.url` points at the in-cluster git-server. The GR
  is the discovery record: app name (`metadata.name`), worktree basename
  (URL path basename), tracked branch (`spec.ref.branch`). `Reconcile()`
  performs one sync tick and returns `RequeueAfter=POLL_INTERVAL` (default
  2s — cadence unchanged). The workqueue guarantees an app is never ticked
  concurrently with itself; `MaxConcurrentReconciles` (~4) bounds cross-app
  parallelism. A GR appearing/disappearing starts/stops its tick stream with
  zero lifecycle code.
- **Static Deployment `git-auto-sync`** in `manifests/dev-loop/base`
  (untemplated, like git-deploy-controller): mounts `/workspaces` hostPath,
  config via env, health probes. Both apply paths (up step 11a SSA and the
  Flux `flywheel-dev-loop` Kustomization) pick it up from the same manifest
  dir.
- **`manifests/per-app-template/git-auto-sync.yaml.tmpl` is deleted**;
  `flywheel add app` renders only GitRepository + build-config +
  ImageRepository/ImagePolicy.

### The race-free tick

Every step is either branch-name-addressed or verified-and-rolled-back;
poison cannot reach a push:

1. **Snapshot.** `symbolic-ref --short HEAD` → checked-out branch `B`
   (detached/empty → skip tick). Snapshot all `refs/heads/*` shas
   (`for-each-ref`). Resolve `L = refs/heads/B`. **All decisions use `L`,
   never `HEAD`.**
2. **Branch-follow.** If `gr.spec.ref.branch != B`: ensure the
   `kustomize.toolkit.fluxcd.io/reconcile: disabled` annotation, then patch
   `spec.ref.branch = B` (same monotonic intent signal as today; the spec
   value arrives free with the reconciled object, so no LAST_BRANCH state).
3. **Fetch** bare `B` → remote head `R`. Objects only; no local ref update.
4. **Integrate** (bare strictly ahead: `L` ancestor of `R`) — one of the two
   worktree-mutating paths (with divergence-rebase, step 7); both share the
   same post-verify + rollback guard:
   - dirty-guard (same data-loss semantics as sync.sh, including the
     issue-#4 exit-code>1 distinction);
   - re-verify `symbolic-ref == B` immediately before `reset --hard R`;
   - **post-verify**: re-read `symbolic-ref` and diff the ref snapshot. If
     any ref other than `refs/heads/B` moved (a checkout won the microsecond
     window), roll the moved ref back from the snapshot and abort the tick —
     the next tick re-runs cleanly on the new branch.
5. **Push** (worktree ahead): explicit sha —
   `push --force-with-lease=B:R <url> L:refs/heads/B`. Pushing the sha
   captured in step 1 (not a ref name) means later state changes cannot
   substitute content. Fallbacks as today: plain push for a brand-new branch.
6. **Poke** `reconcile.fluxcd.io/requestedAt` on the GR when the bare head
   changed (our push, or a fast-forward we integrated) — parity with
   sync.sh's `trigger_reconcile`.
7. **Genuine divergence** (neither ancestor): rebase, abort-on-conflict,
   loud log, long requeue — parity with today's stall behavior. The rebase
   mutates the worktree, so it carries the same re-verify + post-verify guard
   as integrate (step 4): a checkout racing the rebase would move the wrong
   branch, and the post-verify rolls it back and aborts before the push.

### Hygiene / permissions

- `syscall.Umask(0)` at process start plus `core.sharedRepository=0777` (the
  sync.sh startup retro-fix is kept): files the container writes as root stay
  host-writable, fixing the EACCES class even for *legitimate* fast-forwards.
- Port `heal_index_if_corrupt` (issue #4).
- Every git exec runs under a ~30s context timeout so one hung app cannot
  wedge its worker slot.

### Error handling / observability

- A failing tick returns an error → controller-runtime backoff **for that app
  only**.
- Log parity with sync.sh (`kubectl logs` stays the debugging surface):
  branch switches, patches, fast-forwards, pushes, stalls — one line each,
  app-prefixed. The post-verify rollback logs at warning with both refs; it
  should be near-never and we want to see it if it isn't.
- `/healthz` = process up; `/readyz` = informer cache synced. A single wedged
  app must not flip readiness (a restart would land all apps in the same
  state), so no per-app tick outcome feeds readiness — cache sync is the only
  gate.
- **Legacy interlock**: while a `git-auto-sync-<app>` Deployment exists in
  flywheel-system, the reconciler skips that app and warns (once per app) —
  prevents the two-writers-on-one-worktree hazard during migration.

## API / data model changes

No Kubernetes API types added or changed. Concretely:

- New binary: `cmd/git-auto-sync` (env config, following
  git-deploy-controller's pattern):

  ```
  WORKSPACES_MOUNT   hostPath worktrees mount        (default "/workspaces")
  GIT_SERVER_URL     in-cluster git-server base URL  (default the svc DNS)
  BUILDER_NAMESPACE  namespace of per-app GRs        (default "flywheel-system")
  POLL_INTERVAL      tick cadence                    (default "2s")
  MAX_CONCURRENT     reconcile parallelism           (default "4")
  HEALTH_PROBE_ADDR  healthz/readyz bind             (default ":8081")
  ```

- New package `internal/appsync`:

  ```go
  // Ticker performs one race-free sync pass for one app worktree.
  type Ticker struct {
      Dir     string        // worktree path
      BareURL string        // in-cluster bare repo URL
      Flux    FluxPatcher   // branch patch + reconcile poke
      ExecTimeout time.Duration
      Logf    func(string, ...any)
  }
  func (t *Ticker) Tick(ctx context.Context, trackedBranch string) (TickResult, error)
  ```

  plus a `Reconciler` (controller-runtime) that resolves GR → `Ticker` and
  enforces the legacy interlock.

- RBAC (`manifests/dev-loop/base/rbac.yaml`), existing `git-auto-sync` Role in
  flywheel-system:
  - `gitrepositories`: `get, patch` → `get, list, watch, patch`
  - new rule: `apps/deployments`: `get, list, watch` (legacy interlock)

- Manifests: add `manifests/dev-loop/base/git-auto-sync.yaml` (Deployment);
  delete `manifests/per-app-template/git-auto-sync.yaml.tmpl`; drop the
  template's entry from the per-app kustomization and `add app` rendering
  (`internal/cli/add/app`).

- goreleaser: the `git-auto-sync` image ids switch from script-copy to Go
  build; image name and `{{ .Tag }}` templating unchanged.

## Migration plan

- **New apps**: `flywheel add app` stops rendering the sidecar; nothing else
  to do.
- **Existing client repos** (per-app files are never re-rendered by
  flywheel): doc-only migration — release note + guide entry instructing
  `git rm builders/base/<app>/git-auto-sync.yaml` in the gitops repo; Flux
  (`client-builders`, `prune: true`) removes the Deployment. The interlock
  protects anyone who migrates late: old sidecar keeps working (with its
  known race) and the new controller stays hands-off until the file is
  removed, so there is never a two-writer window.
- **e2e**: always a fresh cluster; scenarios pick the new controller up via
  the rebuilt `:ci` image automatically. efq-gitops: one manual PR.
- **Rollback**: revert the flywheel release; old binaries re-render sidecars
  on `add app`. Client repos that already deleted the sidecar file re-add it
  by re-running `add app` or restoring the file from git history.

## Test plan

- **Unit — `internal/appsync`**: ticks against real on-disk git repos
  (selfsync test style): follow/patch, push-guard, fast-forward, rebase,
  conflict-stall, heal-index. Deterministic race injection via test hooks at
  the guard points (a `testHook func(stage string)` invoked between steps):
  - checkout lands between snapshot and fetch → tick no-ops (decisions were
    against `refs/heads/B`);
  - checkout lands before `reset --hard` → re-verify skips the integrate;
  - checkout lands after `reset --hard` (simulated by moving HEAD in the
    hook) → post-verify rolls the wrong ref back and aborts the tick;
  - root-perms: worktree files written through the tick remain writable by
    another uid (asserts the umask effect on modes).
- **Unit — reconciler**: fake-client tests (existing controller idiom):
  GR discovery/filtering, RequeueAfter, legacy-interlock skip + single warn,
  missing-worktree backoff.
- **e2e**: scenarios 2 and 4 unchanged (they exercise the switch legs ~4
  times per nightly). **New stress scenario**: rapid-fire ~10 app branch
  flips at sub-second gaps, then assert (a) served text converges to the
  final branch's content, (b) bare `main` sha == worktree `refs/heads/main`
  sha (no poison), (c) `index.html` still writable by the runner user (no
  root-owned files).
- **Manual**: dogfood on a live cluster (suspend `flywheel-dev-loop` per the
  usual iteration flow), flip branches by hand, watch controller logs.
- **Acceptance bar (closes #86)**: unit races green, stress scenario green,
  and **3 consecutive green `e2e-full` dispatches** with the warm-leg latency
  gates (#111) unchanged.

## Open questions

1. **URL filter predicate** — filter app GRs by `spec.url` prefix
   (`http://git-server.<ns>.svc...`) or simply "all GitRepositories in
   BUILDER_NAMESPACE" (true today; the filter defends against future non-app
   GRs)? Leaning: prefix filter, it is one line.
2. **Shared git runner** — extract the small `run/output` git helpers from
   `internal/selfsync` into a shared package, or duplicate (~40 lines) in
   `internal/appsync`? Leaning: duplicate; extraction touches selfsync in a
   PR that otherwise doesn't.
3. **Stress scenario placement** — new `scenario-6-branch-stress.sh` in the
   nightly-only band (scenarios 2-4 rot silently per-PR; per-PR CI runs only
   1+5) or bolted onto scenario-2? Leaning: new scenario-6, nightly-only,
   with a note in the e2e docs.
4. **Requeue vs. dedicated stall requeue** — is 30s the right conflict-stall
   requeue (sync.sh used 30s sleep)? Cosmetic; default to parity.
