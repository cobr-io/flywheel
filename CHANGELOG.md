# Changelog

All notable changes to Flywheel will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and Flywheel adheres to [semver](https://semver.org/) starting at v1.0.0.
During the v0.x phase no compat promise is made between minor versions
(see design § Versioning).

## [Unreleased]

### Added (2026-06-28, flywheel-free bring-up — committed vanilla Flux entrypoint)

- **`flywheel init` now scaffolds a committed `clusters/local/flux-system/`
  entrypoint** so the same local cluster can be brought up with stock Flux and
  **zero `flywheel` binary** (you forgo the fast dev loop). Previously the repo
  had no committed Flux entrypoint at all — `apps/` and `infra/` were plain
  Kustomize but inert — so the gitops repo only worked through flywheel. This
  removes that implicit lock-in.
  - Five plain-Flux files: a `GitRepository/flux-system` → your GitHub remote on
    `main`, `client-infra`/`client-apps` `Kustomization`s over
    `infra/overlays/local` + `apps/overlays/local`, the `apps` namespace, and a
    `kubectl apply -k`-able aggregator. No mirror, no `flywheel/local-deploy`
    branch, no builders.
  - **Non-interfering:** `flywheel up` renders its own entrypoint at runtime and
    never reconciles `./clusters/`, so the committed tree is inert during normal
    operation and only activates on an explicit `kubectl apply -k`. Applying it
    toggles the cluster's source from the in-cluster mirror to GitHub; the two
    workflows are mutually exclusive.
  - New guide `docs/flywheel-free-bringup.md` documents the four-step bring-up
    and the one caveat — app manifests ship a dev-loop `:0-placeholder` image
    that only flywheel's image automation rewrites, so vanilla pods need real,
    pullable image refs committed.
  - Design: `docs/designs/2026-06-28-flywheel-free-bringup-design.md`.

### Changed (2026-06-18, `add-app` → `add app`)

- **BREAKING: `flywheel add-app` is now `flywheel add app`.** The flat `add-app`
  command was reorganized under a new `add` parent command, anticipating future
  `add <resource>` subcommands (e.g. `add env`). The old `add-app` spelling is
  removed entirely — there is no alias. Flags, args, and behavior are otherwise
  unchanged. Running bare `flywheel add` prints help and exits 2.

### Added (2026-06-17, guard against non-mountable workspace paths)

- **`flywheel init`/`up` now refuse to run from a host path Docker Desktop can't
  bind-mount into k3d** (macOS temp dirs: `/tmp`, `/private/tmp`, `/var/folders`).
  Previously, cloning a gitops repo into `/tmp` produced a cluster whose
  `/workspaces` was empty, so git-auto-sync-self couldn't push the repo and the
  client-* Flux Kustomizations failed with a cryptic "Source artifact not found".
  - The path refusal is **macOS-only** (Linux/CI mount any host path fine) and
    can be overridden with `FLYWHEEL_ALLOW_EPHEMERAL_WORKSPACE=1`.
  - `flywheel up` also **verifies the mount actually bridged** after creating the
    cluster — it checks the gitops repo is visible in-cluster at
    `/workspaces/<repo>` and fails fast with remediation if not. This is
    config-agnostic (catches any Docker Desktop file-sharing misconfig, not just
    temp dirs).
  - `flywheel doctor` (full) warns when the gitops repo is on such a path.
  - New `internal/cli/hostmount` package; new `k3d.WorkspaceVisible` probe.

### Changed (2026-06-17, mirror all images to the local registry; drop k3d side-load)

- **`flywheel up`/`update` now mirror every Flywheel image into the cluster's
  local registry**, for both released (ghcr) and dogfood (override) images, and
  the manifests reference the in-cluster registry pull ref
  (`k3d-<registry>:5000/<name>:<tag>`). Previously only dogfood overrides went
  to the registry while released images were side-loaded with `k3d image
  import`.
  - A released image is pulled to the host then pushed under its immutable
    `:<version>` tag; a dogfood override under a content-addressed
    `:dogfood-<sha>` tag (unchanged).
  - **`k3d image import` is gone entirely** — no per-node side-load, which could
    miss a node or be GC-evicted (issue #14). Every node now pulls on demand
    from the registry, including released images. This also removes the
    requirement that a node reach ghcr (and have pull credentials) at schedule
    time, so private release images and offline-after-pull work.
  - `imagepin.EnsureInCluster` lost its `clusterName` parameter and the
    `k3dImport` helper was removed. `add-app` references the registry pull ref
    for default images too (computed, no docker work — `up` already mirrored
    them).
  - Step-9 log wording now reads `mirroring Flywheel images to the local
    registry` and distinguishes released vs dogfood per image.

### Removed (2026-06-10, drop multi-profile / tailscale support)

- **The local TLS "profile" concept is gone.** Flywheel now supports a
  single, hardcoded local TLS setup (`mkcert`); the `tailscale-le-wildcard`
  profile and the whole profile-selection machinery have been removed.
  - `flywheel.yaml`'s `local.profile` field and the `local.tailscale` block
    are no longer part of the schema (and are rejected).
  - `flywheel init --profile` and `flywheel doctor --mkcert` flags are
    removed; mkcert is always a prerequisite, always checked.
  - `manifests/infra/overlays/local-{mkcert,tailscale}` are collapsed into a
    single `manifests/infra/`; the `flywheel-infra` Flux Kustomization now
    reconciles `./manifests/infra` directly.
  - The `up` step-4 profile-switch detection is removed (every `up` is
    additive in v0.1.0, as documented).
  - **`flywheel clean --crds` is removed.** Its only purpose was reaping the
    cert-manager / tailscale-operator CRDs the tailscale profile installed;
    mkcert installs no operator CRDs, so the flag was orphaned. `flywheel
    clean` (orphaned-PVC cleanup) is unchanged.

### Changed (2026-06-02, add-app worktree decoupling + cobra CLI)

- **The CLI is now built on cobra** (behaviour-preserving migration of the
  hand-rolled subcommand dispatch). Globals (`--no-color`, `-v/--verbose`) are
  persistent flags. New: `flywheel completion <shell>` plus dynamic argument
  completion. The retired `new` command is removed (use `init`).
- **`flywheel add-app` now takes a worktree `<dir>`, not an app `<name>`.** The
  directory (a child of `workspaces_root`; bare name, relative, or absolute
  path) drives the physical bindings — the `/workspaces` mount, the bare-repo
  URL, `GitRepository.spec.url` — while the **app name** drives logical identity
  (folders, resource names, Ingress host, image). The name is `--name`, else
  **derived** from a project manifest in the directory
  (`package.json` / `pyproject.toml` / `setup.cfg` / `go.mod` / `Cargo.toml` /
  `composer.json` / `pom.xml` / `*.gemspec`), else the directory name.
  - **Behaviour change:** when a manifest declares a name it now wins over the
    directory name — pass `--name` to pin. `add-app <dir>` where the directory
    has no manifest name still scaffolds exactly as before.
  - add-app now validates that `<dir>` exists and is a direct child of
    `workspaces_root`, closing a silent-failure mode (a wrong name used to just
    never build). `<dir>` tab-completes to the available worktrees.
- **`make install`** builds a version-stamped binary and installs shell
  completions; **`flywheel version`** now reports `BuildVersion` (the
  git-describe stamp) instead of a hardcoded string. See
  [design](docs/designs/2026-06-02-add-app-worktree-decoupling-design.md).

### Changed (2026-06-01, BuildKit builder)

- **The local builder is now BuildKit, not Kaniko.** A warm rootless
  `buildkitd` Deployment (+ cache PVC + Service) runs in `flywheel-system`
  (`manifests/dev-loop/base/buildkitd.yaml`); each observed commit creates a
  thin `buildctl` client Job that drives it. The build cache lives in the
  daemon's snapshot store, so a code-only change reuses the dependency layer
  instantly instead of paying Kaniko's ~13s cached-layer-extract + ~5s
  layer-push tax on every build. See
  [design](docs/designs/2026-06-01-buildkit-builder.md).
  - **Measured on a heavy multi-stage Go image (paritytest):** warm build
    (code-only change) **38s → 12s** (the `buildctl` step is ~7s — just the
    `go build`; the dep layer is a daemon-cache hit); cold **45s → 32s**;
    end-to-end commit→pod warm **~9-18s** (the spread is the deploy back-half,
    not the build). On a trivial single-layer image there's no difference —
    the win is layer-cacheable real Dockerfiles, which is the actual client
    case.
  - Rootless (no privileged container); the insecure k3d registry is handled
    per-build by `registry.insecure=true` on the buildctl output, so buildkitd
    needs no per-client config. The build container keeps the name `kaniko` so
    the build-Pod scan poke (above) is unchanged. `pods` get/list/watch was
    already added for that poke; no new RBAC here. The build CPU ceiling now
    lives on the daemon (the daemon does the work) as a fixed `cpu: "4"` limit
    on the `buildkitd` Deployment — there is no `BUILD_CPU_LIMIT` env/flag
    anymore (the parity-loop entry below that introduced it is superseded by
    this one); a configurable `build.cpu_limit` knob is a follow-up.
    `moby/buildkit` is pulled on demand (offline pre-import is a follow-up).
  - Removed: `kaniko-cache-pvc.yaml`, the Kaniko Job template (now
    `templates/build-job.yaml` emitting buildctl). The Kaniko engine was
    removed outright, not kept behind a switch.

### Changed (2026-06-01, parity-loop latency)

> **Engine-specific details below are superseded by the BuildKit builder entry
> above** (same day). The Kaniko latency figures, the `kaniko`-container scan
> timings, and the `BUILD_CPU_LIMIT` knob describe the pre-BuildKit engine and
> no longer reflect shipped behaviour. The event-trigger and dependency-requeue
> work (the bulk of this entry) is engine-independent and still stands.

The local commit-to-pod loop was ~25-40s with an intermittent ~40s
outlier (~1 run in 4). It is now **~11-15s, consistently, with the
outlier removed** — measured on a live k3d cluster (dogfood images,
trivial nginx app). The Kaniko build (~9-11s) now dominates; the rest of
the loop is a few seconds. Three changes, each landed only after the root
cause was confirmed by per-controller tracing — not inference:

- **Event-trigger the two hops that were genuinely poll-bound** (Flux's
  `reconcile.fluxcd.io/requestedAt`; best-effort, so a missed poke just
  falls back to the normal interval and reconciliation/parity is
  unchanged):
  - `git-auto-sync` (`scripts/git-auto-sync/sync.sh`) annotates its
    GitRepository whenever a push or fast-forward moves the bare-repo head
    (app source re-fetches on commit; gitops source re-fetches when the
    image-tag bump lands), and the gitops/self sync also pokes the
    `client-apps` Kustomization — but only *after* waiting for the source
    artifact to advance to the new commit, so kustomize-controller applies
    the new revision on that trigger instead of reconciling the stale one.
    (`KUSTOMIZATION_NAME` env on the self sync; `kustomizations` get/patch
    on the `git-auto-sync-flux-system` Role.)
  - A new `BuildJobReconciler` in the image-builder-controller pokes the
    matching ImageRepository to scan the instant a build Job succeeds.
    Bumped via a JSON **merge patch** of just the annotation (a
    read-modify-write Update races image-reflector's own status writes and
    loses with "the object has been modified"). `imagerepositories`
    get/patch added to the controller ClusterRole.
  - **Deliberately NOT poking the ImageUpdateAutomation.** An earlier
    revision did, with `APIReader`/advance-tag machinery; tracing proved it
    dead weight — the IUA already self-reconciles every 5s and commits the
    bump within ~8s of the build regardless, and the poke's policy snapshot
    raced the reflector so it never fired usefully. That code and its
    `imagepolicies`/`imageupdateautomations` RBAC were removed.
- **The ~40s outlier was a `dependsOn` requeue, not a poll interval.**
  Per-controller tracing of a captured outlier showed: every Kustomization
  flips `Ready=Unknown` for ~260ms on *each* routine interval reconcile
  (even a no-op), and an image-bump revision fans out a reconcile to all
  dependents at once. When that fan-out coincides with a transitive
  dependency's 260ms blink, the dependent hits kustomize-controller's
  `--requeue-dependency` backoff — **a fixed 30s constant, independent of
  every `interval`.** Two changes attack it:
  - **A — `--requeue-dependency=30s→2s`** on kustomize-controller. Caps
    the worst case at 2s if the race still hits. The embedded Flux
    `install.yaml` stays **pristine upstream** (clean re-vendor); the flag
    is injected by a programmatic transform in `flux.Install` at apply
    time (`internal/cli/flux`), guarded by a unit test that fails if a
    future re-vendor pre-sets the flag or drops the kustomize-controller
    Deployment.
  - **C — raise the mirror-sourced tiers' interval 10s→5m**
    (`flywheel-dev-loop`, `flywheel-infra`). They change only on
    `flywheel up` (which applies them directly), so a 10s poll bought
    nothing but blink frequency; 5m cuts the coincidence window ~30×. The
    client tiers stay at `flux.interval_local` so client edits stay
    responsive.
- **Verified on the live cluster** (changes delivered through `flywheel
  up` → in-cluster mirror, nothing hand-applied): **14 spaced commits all
  10.8-15.3s, zero dependency requeues, zero outliers.** Rapid-fire (6
  commits ~3s apart) still triggered 3 dependency requeues — but each cost
  ~2s (A) instead of 30s, so the cluster still converged to the newest
  commit +10s after the last commit. So C makes the race rare; A makes it
  cheap when it still happens.
- **Scan poke now fires on the kaniko *container* exit, not the Job's
  Complete condition.** The image lands in the registry the instant kaniko
  exits 0, but the Job isn't marked Complete until kubelet tears the pod
  down and the job-controller observes it — ~4s later, measured, dead on
  the critical path. `BuildJobReconciler` now watches build *Pods*
  (`pods` get/list/watch added to its ClusterRole) and pokes the
  ImageRepository scan off the kaniko container's terminated/exit-0 state.
  Verified: kaniko-done→IUA-push dropped from ~4s to ~0.7-1s. Commit-to-pod
  decomposes to ~11s as: fetch-source initContainer ~3s + container-start
  gap ~2s + kaniko ~3s + (scan→IUA→apply→pod) ~3s. The build *Pod* (two
  serial container cold-starts + build) is now the dominant term — the
  target of the BuildKit work (which can also fold the separate
  git-fetch initContainer into the builder, removing one cold start).
- **Build CPU limit is now a burst ceiling of 2 (was 1).** Builds
  are CPU-bound (~2x faster at 2 vs 1 core); the request stays at 200m,
  so this only consumes cores when they're free and never oversubscribes
  a constrained VM's scheduler. *(Superseded by the BuildKit entry above:
  the ceiling moved onto the `buildkitd` daemon as a fixed `cpu: "4"`
  limit. The `BUILD_CPU_LIMIT` env / `--build-cpu-limit` flag this entry
  described was never shipped; a configurable `build.cpu_limit` knob
  remains a follow-up.)*

### Changed (2026-05-28, second pass)

- **CLI output now styled with ANSI colours + Unicode glyphs**. Step
  headers render bold cyan with `▶`, success lines dim with `✓`,
  warnings bold yellow with `⚠`, errors bold red with `✗`. Honours
  `NO_COLOR` (any value disables), `CLICOLOR_FORCE=1` (forces on),
  and a top-level `--no-color` flag; auto-disables when stdout is
  not a TTY. Hand-rolled (~80 LoC + tests) in `internal/cli/style`;
  no new third-party deps.
- **`flywheel up`'s long waits now show what they're waiting for**.
  Step 10 (Flux Deployments coming up) and step 14 (Flux
  Kustomizations Ready) used to sit silent for minutes; they now
  render a live in-place block per item:

  ```
  ▶ waiting for Flux Kustomizations Ready
    ⠋ client-apps         blocked on: client-infra      5s
    ✓ client-infra        ready                         3s
    …
  ```

  On TTY: cursor-up + clear-to-end + redraw on every poll, ~2s.
  Off-TTY (pipe / CI): a `Detail` heartbeat line every status
  change or every ~20s, with the oldest pending item named. The
  block collapses to one summary line on success. Flux dependsOn
  lag is parsed out of the Kustomization message and surfaced as
  `blocked on: <dep>` so the user sees which link is holding the
  chain. New `style.Waiter` in `internal/cli/style/wait.go`.
- **Default output is now quiet**. Subprocess chatter that used to
  inline into the user-facing log — k3d's `[INFO]/[ERRO]` lines from
  `k3d image import`, docker pull progress, the mkcert install
  banner, client-go's port-forward "Forwarding from …" notices,
  klog `E0000 …` warnings, and the applier's per-resource `ok …`
  chatter on every apply — is hidden in the default run. Pass
  `-v` / `--verbose` to surface all of it for diagnosis.
  Implemented via `style.VerboseWriter(w)` (returns `io.Discard`
  unless verbose) routed through each shell-out callsite +
  `klog.SetOutput(io.Discard)` for client-go warnings. The
  per-resource ok line is gated through `style.OKv` (verbose-only
  OK).
- **Animated progress spinner for one-shot waits**. With subprocess
  chatter hidden, silent steps now show the same braille-spinner
  glyph the Waiter uses, ticking elapsed time in place:

  ```
  ⠹ k3d cluster myapp-local  47s
  ```

  When the step finishes, the spinner line is replaced by a dim
  `✓ k3d cluster myapp-local (1m28s)` summary. On failure it
  becomes a bold-yellow `⚠ <step> failed (1m28s)`. New `style.Spin`
  helper; wired into `flywheel up`'s registry / cluster create,
  per-image k3d image-import, Flux install, dev-loop apply, mirror
  push, namespace + flywheel-config apply, age + local-cert
  secrets, and the bootstrap flux-system apply. Also wired into
  `flywheel down`'s stop and `flywheel destroy`'s
  cluster-and-registry deletes.

  Degraded modes: when stdout isn't a TTY (logs / CI) or `-v` is
  on (where subprocess output would otherwise clash with the in-
  place redraw), `Spin` skips the animation and prints a plain
  step header + final outcome line — no ANSI escapes, log-safe.

### Changed (2026-05-28)

- **The Flux entrypoint (`clusters/local/flux-system/`) is no longer
  rendered into the client gitops repo**. It's bootstrap-only — applied
  once by `flywheel up` step 11d, then Flux reconciles only its
  `sourceRef` targets (the Flywheel mirror + `builders/`, `apps/`,
  `infra/` from the gitops repo). The templates now live under
  `templates/bootstrap/` in the binary; `flywheel up` renders them to a
  tmpdir with runtime values (resolved image refs, embed-cache SHA, repo
  basename) and applies. The tmpdir is removed on exit.
  - Eliminates the git-auto-sync ↔ refresh-overlay race: there's
    nothing in the committed repo for `git-auto-sync` to reset, and no
    "refresh" step that produces uncommitted churn.
  - `.local` edits flow through on the very next `up` (the values feed
    straight from `cfg.Flywheel.Images` into the rendered templates).
  - The user's committed gitops repo now contains only what Flux
    actually reconciles: `flywheel.yaml`, `builders/`, `apps/`,
    `infra/`. Existing clients can `git rm -r clusters/` after
    upgrading; nothing in the binary references that path anymore.
  - Removes `up.refreshFluxOverlay` + the three `renderBuildersKustomization`/
    `renderFlywheelSource`/`renderSelfGitAutoSync` helpers (replaced
    by the embedded templates and `up.renderBootstrap`).
  - Goldens shrink (the `clusters/local/flux-system/` subtree is gone
    from `internal/cli/new/testdata/golden/{mkcert,tailscale}/`).

### Fixed (2026-05-28)

- `flywheel init` now reuses an existing age key at
  `~/.config/flywheel/<client>/age.key` instead of refusing to proceed.
  The age key is a per-developer identity, not a per-cluster artefact —
  destroying and re-initing a cluster must preserve the same key so any
  committed `*.sops.yaml` files in the gitops repo stay decryptable.
  `flywheel destroy` is unchanged (intentionally leaves the key on
  disk); the bug was on the init side. Generation still fires on first
  init or when the key file is absent.
- `flywheel add-app` now merges `flywheel.yaml.local` before resolving
  image refs, so per-developer image overrides flow into the rendered
  per-app git-auto-sync Deployment. Previously dogfood clusters would
  hit `ErrImagePull` on the default ghcr.io ref.
- `image-builder-controller` now sweeps orphaned Kaniko build Jobs
  when their source `GitRepository` is deleted. Cross-namespace owner
  references aren't honoured by Kubernetes garbage collection — the
  Jobs live in `flywheel-system`, the `GitRepository` lives in `apps` —
  so the controller reaps Jobs labelled `app=image-builder,repo=<dead>`
  in its own `Reconcile` on the delete event.
- `image-builder-controller` also reaps the orphan `GitRepository`
  itself when its build-config ConfigMap is gone. Per-app
  `GitRepository`s carry `kustomize.toolkit.fluxcd.io/reconcile:
  disabled` (set imperatively by git-auto-sync to win the Open Issue
  #11 race on `spec.ref.branch`), which also blocks Flux's prune. The
  controller deletes the stranded `GitRepository` directly (the
  annotation only blocks Flux reconciles, not generic API deletes);
  the subsequent delete event fires `IsNotFound` and the Job sweep.
  RBAC was widened to grant `delete` on `gitrepositories` in `apps`.
- New scenario `testdata/scenarios/scenario-5-orphan-job-reaper.sh`
  exercises the full flow on a live cluster (remove builder → assert
  GR pruned → assert Jobs reaped) and is wired into both `run-all.sh`
  and the k3d-e2e CI job.

### Changed (dogfood pass, 2026-05-27)

- `flywheel new` is retired and replaced by `flywheel init [<path>]`:
  no argument initialises the current directory (Name derived from
  `basename(cwd)`); an argument creates / initialises that path. Empty
  or `.git`-only targets are accepted; any other content is refused.
- The binary now `go:embed`s `templates/client-skeleton/`,
  `manifests/`, and `manifests/per-app-template/` — `init`, `up`, and
  `add-app` no longer need a git clone of `github.com/cobr-io/flywheel`
  at runtime. The `FLYWHEEL_REPO_URL`, `--local-build`, and
  `FLYWHEEL_BUILD_ROOT` knobs are gone (removed with `gitcache` and
  `dockermirror`).
- `up` step 3 derives a deterministic commit SHA from the embedded
  asset tree via the new `embedcache` package (no network,
  reproducible).
- `up` step 9 loads the three runtime images via the new `imagepin`
  package: `cfg.flywheel.images.<name>` override or
  `ghcr.io/cobr-io/<name>:<flywheel.version>`; loaded into the
  cluster's containerd via `k3d image import` from the host docker
  store (else `docker pull`). If the default ghcr.io ref isn't
  published and no override is set, hard-fail with a remediation
  pointer (option (c)).
- `up` step 11d refreshes the committed
  `clusters/local/flux-system/{builders-kustomization,flywheel-source}.yaml`
  to reflect resolved image refs + current embed SHA, then applies.
  The committed copy becomes a normal git diff the developer can
  commit.

### Added

- `flywheel add-app <name>` (promoted from Phase 3): renders the
  embedded per-app-template into `builders/base/<name>/` and appends
  the entry to `builders/base/kustomization.yaml`. Honours
  `cfg.flywheel.images.git-auto-sync` so per-developer dogfood image
  overrides flow through.
- `flywheel.images.{git-server,git-auto-sync,image-builder-controller}`
  config block (optional). Natural home for per-developer dogfood
  overrides is `flywheel.yaml.local`.

### Added

- Phase 0 scaffold: repo skeleton, Dockerfiles + scripts (git-server,
  git-auto-sync, image-builder-controller), parameterised
  image-builder-controller reading `flywheel-config` ConfigMap,
  dev-loop manifests (incl. privileged inotify-bump DaemonSet), infra
  overlays (mkcert + tailscale-le-wildcard), per-app template,
  client-skeleton template, CLI skeleton (`doctor` implemented; others
  stubbed), schema validator (v1alpha1), config merger (`.local`
  arrays-replace-wholesale), allocator (~/.config/flywheel/allocations.json),
  goreleaser config, release + test workflows.
- Phase 1: `flywheel new`, `up` (full 15-step pipeline incl. in-cluster
  Flywheel mirror for offline reconcile), `down`, `destroy`, full
  `doctor`. Dev-loop validated end-to-end on k3d (commit → build →
  deploy → serve) plus app-repo and gitops-repo branch switches.
  Embedded Flux v2.8.7 install; client-go SSA applies with
  fieldManager=flux-controller; go-git tag→SHA + cache + in-cluster
  mirror push.
- `flywheel up` step 4 (reconcile diff) + step 12 (orphan deletes):
  profile-switch detection categorises changes additive/mutating/
  destructive, gates destructive ops behind `--yes`, and tiers CRDs +
  PVCs out of auto-deletion (never deleted by `up`).
- `flywheel clean [--orphaned] [--crds]`: removes orphaned PVCs; removes
  orphaned CRDs only after a foreign-CR safety check (refuses if any CR
  isn't labeled `app.kubernetes.io/managed-by=flywheel`).
- Dev-loop validation scenarios (testdata/scenarios/) + CI k3d-e2e job.

- Open Issue #11 hardening: the gitops git-auto-sync suspends the
  `flywheel-self` ImageUpdateAutomation for the duration of a branch
  switch (resumes once settled; self-healing on crash/startup), so IAC
  can't commit to the gitops bare repo mid-switch. All four dev-loop
  scenarios — including scenario 4 (both repos on independent feature
  branches simultaneously) — pass on a clean cluster.
