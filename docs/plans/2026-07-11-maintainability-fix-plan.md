# Maintainability fix plan

**Source:** full-codebase maintainability review, 2026-07-11, against main @ affdc5f.
**Status:** ready to execute. Each task below is a self-contained brief sized for one agent session (Opus or Sonnet) and one PR.

## How to use this plan

- **One task = one branch = one PR.** Cut every branch fresh from `origin/main`, named `fix/<task-id>-<slug>` (e.g. `fix/t01-clean-pvc-selector`).
- **Model tag** is the minimum tier: `sonnet` tasks are fully specified and mechanical; `opus` tasks involve cross-cutting judgment or risky refactors. Opus can take any task.
- **Size:** S ≈ under an hour of agent work, M ≈ a session, L ≈ a session with careful review.
- **Do the task as written.** If the code has drifted from the evidence cited (file:line refs are against affdc5f), re-verify before editing; if a task no longer applies, close it with a note instead of improvising.
- Tasks within a phase are independent unless a **Depends** line says otherwise. Phases are priority bands, not strict gates.

### Global gotchas (read before any task)

1. **Two apply paths.** Dev-loop manifests are applied both by `up` step 11a (SSA, via `converge.ApplyDevLoop`) and by the Flux `flywheel-dev-loop` Kustomization rendered from `templates/bootstrap/.../builders-kustomization.yaml.tmpl`. A value changed on one path only gets reverted by the other on a live cluster (Flux `prune: true`, ~5m fallback reconcile). Any task touching `manifests/dev-loop/` or `internal/cli/converge/helpers.go` must patch both paths.
2. **Templates render into client repos.** Changes under `templates/client-skeleton/` only affect *newly initialized* repos; existing clients keep the old files. Note this limitation in the PR description when it applies.
3. **Golden tests.** Any template change requires `go test ./internal/cli/initcmd/... -update` and committing the regenerated `testdata/golden/default/` tree.
4. **Backward compatibility of `flywheel.yaml`.** Existing client files must keep parsing. Never delete a schema field in the same PR that introduces strict parsing; deprecate (accept + validate) instead.
5. **Verification.** `go vet ./... && go test ./...` is the floor for every task. Template tasks add golden regen; manifest tasks add `kubectl kustomize` renders; tasks marked *e2e-relevant* should note in the PR that nightly `e2e-full` is the real proof (per-PR CI runs scenarios 1+5 only).

---

## Task index

| ID  | Title | Model | Size | Phase | Depends |
|-----|-------|-------|------|-------|---------|
| T01 | Fix `clean` deleting all PVCs (missing label selector) | sonnet | S | 0 | – |
| T02 | Strict `flywheel.yaml` parsing | sonnet | S | 0 | – |
| T03 | Make `--wait=false` work (`*bool` sentinel) | sonnet | S | 0 | – |
| T04 | CI guards: snapshot version stamp assert + `:latest` glob | sonnet | S | 0 | – |
| T05 | Client CI scripts: bash-3 compatible + fail closed | sonnet | S | 0 | – |
| T06 | Remove dead self-sync mode from sync.sh | sonnet | M | 0 | – |
| T07 | Vestigial-docs/dead-code sweep | sonnet | S | 0 | – |
| T08 | Fix e2e failure-diagnostics registry port | sonnet | S | 0 | – |
| T09 | `signal.NotifyContext` at CLI root | sonnet | S | 0 | – |
| T10 | `internal/naming` constants package + agreement tests | opus | M | 1 | – |
| T11 | One config loader for all commands | opus | M | 1 | – |
| T12 | Shared exec/git runner + shared git test helper | opus | M | 1 | – |
| T13 | Single producer for the `flywheel-config` ConfigMap | opus | M | 1 | T10 |
| T14 | Flywheel namespace: hard-code behind one global definition | opus | M | 1 | T10, T13 |
| T15 | Apps namespace: make the global default real | opus | M | 1 | T13, T14 |
| T16 | `ApplyDevLoop`: stop ignoring the overlay | sonnet | S | 1 | – |
| T17 | e2e recipe: one source of truth (+ `TIMEOUT_SCALE`) | opus | L | 1 | – |
| T18 | Derive image wiring from `schema.ImageNames` | opus | M | 1 | – |
| T19 | `up.Run` step table (names, not numbers) | opus | L | 2 | T10, T13, T14 |
| T20 | `add app`: transactional ordering + dedup validation | opus | M | 2 | – |
| T21 | Source-modes owning package | opus | M | 2 | T10 |
| T22 | Applier tests + per-object error aggregation | opus | M | 2 | – |
| T23 | Controllers: one poke helper, reconcile tests, preflight re-probe | opus | L | 2 | – |
| T24 | One YAML-editing dialect for client files | opus | L | 2 | – |
| T25 | Doctor severity levels | sonnet | M | 2 | – |
| T26 | Embed/golden completeness guard | sonnet | S | 2 | – |
| T27 | Completion paths: single source | sonnet | S | 2 | – |
| T28 | imagepin: injectable seams + tag-picker dedup | sonnet | M | 2 | – |
| T29 | Shared template render context | opus | M | 2 | T14, T18 |
| T30 | git-deploy-controller: health probes | sonnet | S | 2 | – |

---

## Phase 0 — quick wins (independent, start anytime)

### T01 — Fix `clean` deleting all PVCs `sonnet · S`
**Evidence:** `internal/cli/clean/run.go:34-39` — the doc comment says "deletes PVCs … **labeled managed-by=flywheel**", but the code calls `a.ListUnstructured(ctx, gvr, ns)`, and `ListUnstructured` lists with bare `metav1.ListOptions{}` (`internal/cli/applier/applier.go:257-269`). Every PVC in the namespace is deleted, including app PVCs created by Flux.
**Change:** list label-scoped. The applier already has `ListByKindLabeled` (label-selector list, `applier.go` just below `ListUnstructured`) and converge exports the selector (`converge/prune.go:20`, `app.kubernetes.io/managed-by=flywheel`). Either use `ListByKindLabeled` or add a `ListUnstructuredLabeled` variant — do not widen `ListUnstructured`'s signature (9 packages import applier).
**Accept:** `clean` only deletes labeled PVCs; a unit test (fake dynamic client) proves an unlabeled PVC in the same namespace survives. This is the package's first test — keep it minimal.
**Verify:** `go test ./internal/cli/clean/...`.

### T02 — Strict `flywheel.yaml` parsing `sonnet · S`
**Evidence:** `internal/cli/schema/schema.go:217` uses `yaml.Unmarshal` (sigs.k8s.io/yaml, non-strict). A typo'd key (`gitserver:` for `git_server:`) silently becomes defaults — the failure class the code itself warns about at `schema.go:322`.
**Change:** switch `Parse` to `yaml.UnmarshalStrict`. Run the full test suite; fix any fixture that relied on leniency. Check that `flywheel.yaml.local` files go through the same `Parse` after `MergeYAML` (they do — merge happens on raw bytes first).
**Accept:** unknown top-level and nested keys produce a parse error naming the key; existing golden `flywheel.yaml` and all e2e fixtures still parse.
**Watch out:** coordinate with T14 — never remove a schema field while strict parsing is live; deprecate instead (see T14).
**Verify:** `go test ./...`; add a `Parse` test with a misspelled key.

### T03 — Make `--wait=false` work `sonnet · S`
**Evidence:** `cmd/flywheel/commands.go:161` wires `--wait` (default true) into `up.Options.Wait`, but `up.Run` forces the zero value back to true (`internal/cli/up/up.go:69-75`) — `--wait=false` is a silent no-op. The correct pattern (`*bool` sentinel) is used for `--clone/--no-clone` at `commands.go:147-151`.
**Change:** make `Options.Wait` a `*bool` like `Clone`; nil = default true. Update the dispatcher and any test constructing `Options`. Delete the apologetic comment.
**Accept:** `up --wait=false` skips the wait steps (assert via unit test on options resolution, not e2e); default behavior unchanged.
**Verify:** `go test ./internal/cli/up/... ./cmd/...`.

### T04 — CI guards: version stamp + `:latest` glob `sonnet · S`
**Evidence:** (a) the ldflags symbol `github.com/cobr-io/flywheel.BuildVersion` is spelled independently in `Makefile:11` and `.goreleaser.yaml:42`; a wrong symbol is a silent no-op and has shipped v0.0.0-dev releases twice (see the incident comments at `.goreleaser.yaml:35-41`). (b) `test.yml:27-35`'s `:latest` invariant enumerates the four Dockerfiles by name — a fifth Dockerfile would be silently unscanned.
**Change:** (a) add a CI step (in `test.yml`, cheap job) that runs `goreleaser release --snapshot --clean --skip=publish,docker` (or `--snapshot` with docker skipped — check flags against the pinned goreleaser version) and asserts `dist/**/flywheel version` does **not** report `v0.0.0-dev`. (b) replace the Dockerfile enumeration with a `Dockerfile.*` glob.
**Accept:** deliberately breaking the `-X` symbol locally makes the new CI step fail; `git ls-files 'Dockerfile.*'` shows all files covered by the `:latest` check.
**Verify:** run the snapshot command locally once (documented as safe in the release-process notes: `goreleaser release --snapshot --clean` tests the pipeline without a tag).

### T05 — Client CI scripts: portable + fail closed `sonnet · S`
**Evidence:** `templates/client-skeleton/scripts/ci/check-local-only.sh:36` uses `declare -A` (bash 4+) under `set -euo pipefail` (line 18), while `.pre-commit-config.yaml.tmpl:32-35` runs it via PATH `bash` with `always_run: true` — on stock macOS (bash 3.2) **every commit in a client repo fails** with `declare: -A: invalid option`. `check-sops-shape.sh` wraps yq calls in `2>/dev/null || true`, so its plaintext-Secret rule silently passes on malformed YAML (fail-open), and computes a `doc_count` that is never used.
**Change:** rewrite `check-local-only.sh` without associative arrays (newline-delimited list + `grep -Fx`, or two parallel arrays); make `check-sops-shape.sh` treat yq errors as failures with a clear message (drop `|| true`, keep a readable error path); delete `doc_count`. Both scripts must keep passing `bash 3.2` (`/bin/bash` on macOS) and `shellcheck`.
**Accept:** `/bin/bash templates/client-skeleton/scripts/ci/check-local-only.sh` runs on macOS system bash against a fixture repo; a malformed YAML file makes `check-sops-shape.sh` exit non-zero. Golden copies regenerate (`internal/cli/initcmd/testdata/golden/default/scripts/ci/` mirrors these byte-for-byte).
**Watch out:** shipped scripts in existing client repos won't update (global gotcha 2) — say so in the PR.
**Verify:** `go test ./internal/cli/initcmd/... -update && go test ./...`; `shellcheck` both scripts.

### T06 — Remove dead self-sync mode from sync.sh `sonnet · M`
**Evidence:** `scripts/git-auto-sync/sync.sh` (452 lines) still carries the full `AUTO_FOLLOW_BRANCH=false` gitops/self-sync mode: drift correction (lines ~286-316), the `IUA_SUSPENDED` state machine (~173-186, 255-284), `DEPLOY_BRANCH_ANNOTATION` (line 68), and the Kustomization-poke half of `trigger_reconcile` (~226-252). Nothing sets `AUTO_FOLLOW_BRANCH`, `IUA_NAME`, or `KUSTOMIZATION_NAME` anywhere in `manifests/` or `templates/` (verified by grep — only an rbac.yaml comment mentions them). That job moved to Go (`internal/selfsync`, whose doc header says it replaced the sidecar for the gitops repo).
**Change:** delete the dead mode: the `AUTO_FOLLOW_BRANCH` branch, IUA suspend/resume functions and state, the deploy-branch annotation reading, and the kustomization-poke path of `trigger_reconcile`. Keep everything the per-app sidecar uses (clone/fetch loop, `heal_index_if_corrupt`, the data-loss guard, push-guard, the ImageRepository poke if per-app pods use it — check what env the per-app template sets: `manifests/per-app-template/git-auto-sync.yaml.tmpl`). Add a header comment: "gitops/self-sync moved to internal/selfsync (git-deploy-controller); this script serves per-app sidecars only."
**Accept:** script under ~300 lines; every remaining env var is set by `git-auto-sync.yaml.tmpl` or has a used default; `bash -n` passes; rebuilt `Dockerfile.git-auto-sync` image runs the happy path (clone + serve loop) against a local fixture if feasible, otherwise flag as *e2e-relevant*.
**Verify:** grep proves no template/manifest references the deleted vars; nightly e2e scenarios 2-4 are the true regression net — note in PR.

### T07 — Vestigial-docs/dead-code sweep `sonnet · S`
**Evidence & change list** (each item is a small, independent edit — do them all):
1. `internal/cli/applier/applier.go:205` — doc says DeleteResource is "used by `flywheel up` step 12"; step 12 and the destructive-reconcile feature were removed. Fix the comment (clean uses it).
2. `internal/cli/converge/config.go:2-3` — package doc references "future commands (e.g. `update`)"; `update` was deleted (PR #54). Fix.
3. `internal/cli/up/up.go:63` — "Run is the 15-step pipeline" while steps 4/12 don't exist and step 8 is a placeholder. Reword to describe reality without renumbering anything (T19 does the real fix).
4. `internal/cli/initcmd/init.go` — delete the dangling `buildState` doc comment above `gitCmd` (references the removed `update` 3-way merge); delete the no-op `stripStatePathsForGolden` stub if truly unused.
5. `Dockerfile.git-auto-sync:3-4` — comment points at a nonexistent `K3S_IMAGE` Makefile var; the real pin is `k3s_image` in `templates/client-skeleton/flywheel.yaml.tmpl:20` / `schema.go:75`. Fix the pointer.
6. `manifests/per-app-template/git-auto-sync.yaml.tmpl` — pod label `worktree: {{ .AppName }}` while env uses `{{ .Worktree }}`; make the label use `{{ .Worktree }}` (misleading exactly when app ≠ worktree).
7. `manifests/dev-loop/base/*.yaml` stale `:v0.1.0` image tags (current release v0.2.0): replace with a self-describing placeholder tag (e.g. `:rewritten-by-flywheel-up`) so a rewrite gap fails loudly as ImagePullBackOff with a googleable tag instead of silently running an old version. **Two-apply-paths check:** both rewriters (`renderDevLoopKustomization` and the bootstrap templates' `images:` blocks) must keep matching the base image *names* — tags are what's rewritten; confirm `kubectl kustomize` output of both paths still resolves every image.
**Accept:** all seven edits in one PR; goldens regenerated for item 6; `kubectl kustomize manifests/dev-loop/base/` renders (with placeholder tags).
**Verify:** `go test ./... `; grep shows no remaining "step 12"/"update command" references.

### T08 — Fix e2e failure-diagnostics registry port `sonnet · S`
**Evidence:** `testdata/scenarios/lib.sh:125` curls `http://localhost:${REGISTRY_PORT}/v2/…` in `dump_diag`. CI hard-codes `REGISTRY_PORT=50001` (`test.yml:204`, `e2e-full.yml:150`) even though the same job proves init reallocates squatted ports; `scripts/e2e.sh:85` sets `REGISTRY_PORT=0` with a comment wrongly claiming it's only a presence-guard. So the diagnostics query the wrong port exactly when you need them.
**Change:** derive the port inside `lib.sh` from the client repo's `flywheel.yaml` (`awk`/`yq` on `cluster.registry_port` — the workflows already awk `http_port` at `test.yml:153`; copy that pattern). Drop the `REGISTRY`/`REGISTRY_PORT` `:?` requirement guards (lib.sh:15-16) if no longer needed — `REGISTRY` is used nowhere but the guard.
**Accept:** `dump_diag` works with no exported REGISTRY_* vars; the three callers (both workflows, e2e.sh) drop their exports.
**Verify:** run `scenario-doctor.sh` locally (cluster-free) to confirm lib.sh sources cleanly; shellcheck.

### T09 — `signal.NotifyContext` at the CLI root `sonnet · S`
**Evidence:** every command passes `context.Background()` (`cmd/flywheel/commands.go:152,177,205,352`); no signal handler exists, so all threaded `ctx.Done()` branches and `exec.CommandContext` cancellation are dead during `up`'s multi-minute waits. Controllers do it right (`ctrl.SetupSignalHandler()`).
**Change:** create one `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` in `main.go` / root command `PersistentPreRun`, thread it to the per-command `ctx`. Also fix the one bare `time.Sleep(2s)` retry loop at `internal/cli/up/helpers.go:36` to select on ctx (the ctx-aware pattern already exists in converge's waiters).
**Accept:** Ctrl-C during a wait loop returns promptly with a "canceled" error instead of dying mid-write; second Ctrl-C hard-kills (default NotifyContext behavior).
**Verify:** `go test ./...`; manual: `flywheel up` against no docker, Ctrl-C during the wait, observe clean exit.

---

## Phase 1 — foundations

### T10 — `internal/naming` constants package + agreement tests `opus · M`
**Evidence:** no constants package exists. `DeployBranchAnnotation = "flywheel.cobr.io/deploy-branch"` is declared in both `internal/cli/usecmd/usecmd.go:47` and `internal/selfsync/flux.go:19`. The managed-by marker is spelled three ways: selector string `"app.kubernetes.io/managed-by=flywheel"` (`converge/prune.go:20`), map literal (`converge/helpers.go:148`), map literal (`up/helpers.go:185`) — and the orphan-prune's safety depends on them agreeing. `flywheel/local-deploy` has no constant (env default `cmd/git-deploy-controller/main.go:55` + 3 template/manifest literals). `"flux-system"` at 10 non-test sites in 6 files; `"flywheel.yaml"`/`.local` at 11 sites in 9 files; the in-cluster git-server URL spelled in ≥4 places; `reconcile.fluxcd.io/requestedAt` in 3 Go declarations + bash.
**Change:** create `internal/naming` (importable by both `internal/cli/*` and `internal/controller`/`internal/selfsync` — it must stay dependency-free): managed-by key/value + a `Selector()` and `Labels()` helper; deploy-branch annotation; deploy branch name; flux namespace; flywheel namespace (see T14); config file names; state file name; reconcile-request annotation; `GitServerURL(namespace)` builder. Migrate the Go call sites. For strings that also live in templates/manifests (deploy branch, managed-by, git-server URL), add **agreement tests**: render the relevant template with test values (the `TestRenderBootstrap_*` pattern exists in converge) or read the static manifest, and assert the literal equals the constant — so a drift fails `go test`, not a live cluster.
**Non-goals:** don't move namespace *values* yet (T14/T15); don't touch bash (sync.sh gets a pointer comment only).
**Accept:** the duplicated `DeployBranchAnnotation` const is deleted from one of the two packages (one imports the other or both import naming); grep finds each identity string in exactly one Go location + templates; agreement tests fail if a template literal is edited.
**Verify:** `go test ./...`; `grep -rn 'flywheel.cobr.io/deploy-branch' --include='*.go' internal/ cmd/ | grep -v _test | wc -l` → 1.

### T11 — One config loader `opus · M`
**Evidence:** read→merge(`flywheel.yaml.local`)→parse is implemented 6× with divergent semantics: `converge.LoadConfig` (`converge/config.go:18`, the only one that validates), `add/app/app.go:425-449` (injects its own defaults), `publishapp.go:111`, `usecmd.go:199` (adds cluster.name check), `doctor/workspace.go:92` (no validation), `down/down.go:100` (partial validation) — and `cmd/flywheel/commands.go:400-416` `readClusterConfig` doesn't merge `.local` at all, so `flywheel clean` ignores a `.local` cluster-name override that `flywheel use` honors. `down` also re-implements `portheal.go:183-192`'s allocations-path resolution inline (`down.go:61-72`).
**Change:** one loader in a leaf package (either extend `converge.LoadConfig` or move it to `internal/cli/config` — pick whichever avoids import cycles; config is likely the right home) with options: `Validate` (full/local/none), `RequireCluster`, and defaults applied in **one** place (fold `add app`'s `Namespaces.Apps`/`IntervalLocal` defaulting into schema accessors or the loader — not per-command). Migrate all 7 call sites. Extract the allocations-path resolution into one function used by both portheal and down.
**Behavior change to flag in the PR:** `clean` (via `readClusterConfig`) will start honoring `flywheel.yaml.local`. That is the correct behavior; call it out.
**Accept:** exactly one function reads `flywheel.yaml*`; table test covering the option matrix; the divergence test — a `.local` cluster-name override — is honored by `use`, `clean`, `down` alike.
**Verify:** `go test ./...`; grep: `MergeYAML` called from one non-test file.

### T12 — Shared exec/git runner + shared git test helper `opus · M`
**Evidence:** 39 non-test `exec.Command(Context)` sites across 12 packages; ≥5 hand-rolled git helpers with divergent error formats — `deploybranch.runGit` (`deploybranch.go:246`) and `selfsync.run/output` (`selfsync.go:267-281`) are near-identical including a duplicated `gitEnv()`; `initcmd.gitCmd` wraps with `"%v: %v\n%s"`, breaking `errors.Is/As` on init's error path. 11 sites use `exec.Command` without context (`worktree.go:34,132,143,151,155`; `usecmd.go:171,189`; `gitcheckout.go:166`; `converge/bootstrap.go:130`; `initcmd:303,458`). Test seams follow three conventions (pure argv builders / mutable package vars / real-git temp repos), and the `initRepo` git-bootstrap helper is copy-pasted across ≥4 test files.
**Change:** small `internal/execx` (or `internal/cli/execx`): `Run(ctx, dir, name, args...) (stdout, stderr, error)` with one error format (`%w` + trimmed stderr), plus a `Git(ctx, dir, args...)` variant carrying the shared `gitEnv`. Migrate the git helpers first (deploybranch, selfsync, gitcheckout, initcmd, worktree, usecmd, converge) — the non-git call sites (docker, k3d, mkcert, yq) can migrate opportunistically; don't force a big-bang. Add `internal/testutil` (or `_test` shared package) with the git-repo bootstrap helper and de-duplicate the ≥4 copies.
**Non-goals:** no interface/mocking layer — the codebase's real-git-in-tempdir test style works; keep it.
**Accept:** zero context-less `exec.Command` in git paths; one `gitEnv`; `errors.Is` works through init's git errors (test it); test bootstrap helper used by worktree/deploybranch/selfsync/doctor tests.
**Verify:** `go test ./...`; grep counts before/after in the PR description.

### T13 — Single producer for `flywheel-config` `opus · M` — *depends: T10*
**Evidence:** the ConfigMap is produced twice per `up`: Go (`converge.ApplyFlywheelConfig`, `helpers.go:122-156`, uses `cfg.Namespaces.*`, sets the managed-by label) at the step-11 prelude, then the bootstrap-rendered template (`templates/bootstrap/clusters/local/flux-system/flywheel-config.yaml.tmpl`, hard-codes `namespaces.flywheel: "flywheel-system"`, `namespaces.apps: "apps"`, no label) at step 11d — the later write wins. They have already diverged. Consumers hard-code dotted keys in `manifests/dev-loop/base/image-builder-controller.yaml` (6 `configMapKeyRef`s) and `git-deploy-controller.yaml` (2), read via `internal/controller/config.go`.
**Change:** one producer. Recommended shape: keep the ConfigMap **in the Flux tree** (so Flux owns it and prune bookkeeping stays consistent) but render the template *from the same Go map* — export the key/value map builder from converge (or naming-adjacent package), have `bootstrapValues` inject every key, and delete `ApplyFlywheelConfig`'s direct apply (or keep it only as the pre-Flux bootstrap seed — decide based on whether any step between 11-prelude and 11d reads the ConfigMap; check `git-server` startup). **Flux-prune interplay (global gotcha 1):** if the direct-apply copy is kept, it must carry identical content AND the managed-by label story must be reconciled — Flux-owned objects should not carry flywheel's prune label if `PruneOrphanedMachinery` could reap them when the keep-set changes; check `converge/prune.go`'s denylist/keep-from-this-run logic before choosing.
**Accept:** one place lists the config keys; an agreement test asserts every `configMapKeyRef` key in the two consumer manifests exists in the producer map; non-default `namespaces.apps` in cfg survives a full render (unit-level: rendered template contains the cfg value, not `"apps"`).
**Verify:** `go test ./...`; *e2e-relevant* — note that scenario 1 exercises the ConfigMap path.

### T14 — Flywheel namespace: hard-code behind one global definition `opus · M` — *depends: T10, coordinate with T13*
**Decision (owner-confirmed 2026-07-11):** flywheel's own namespace is **not** user-configurable. It lives in **one** global definition; the ~55 scattered literals get derived from it or guarded by tests. (Apps namespaces stay configurable — that's T15.)
**Evidence:** `schema.Validate` requires `namespaces.flywheel` (`schema.go:309`) but `up` waits/pushes on the literal (`up.go:373,382`), `add app` bakes `git-server.flywheel-system.svc...` into scaffolds (`app.go:343`), controller defaults embed it (`internal/controller/config.go:68`, `cmd/git-deploy-controller/main.go:51`), and 52 literals sit across `manifests/` + `templates/`.
**Change:**
1. `naming.FlywheelNamespace = "flywheel-system"` (T10 package) + `naming.GitServerURL()` derived from it. Migrate the Go literals (up, add/app, converge, controllers' *defaults* — controllers keep env override for tests).
2. Schema: make `namespaces.flywheel` **optional**; if set, it must equal the default — otherwise a clear error ("flywheel's namespace is fixed at flywheel-system; remove `namespaces.flywheel`"). Keep the struct field so strict parsing (T02) still accepts existing client files. Remove the key from `templates/client-skeleton/flywheel.yaml.tmpl` (new clients don't see it).
3. Templates already `.tmpl`-rendered (bootstrap tree, per-app templates): replace the literal with a placeholder fed from the constant via render values. Static manifests (`manifests/dev-loop/base/*.yaml` can't template): keep literals but add one agreement test that walks `manifests/` + rendered templates and asserts the only flywheel-namespace string used is `naming.FlywheelNamespace`.
**Accept:** grep for `flywheel-system` in `*.go` (non-test) hits only `internal/naming`; agreement test covers manifests/templates; old client `flywheel.yaml` with `namespaces.flywheel: flywheel-system` still validates; `namespaces.flywheel: other` errors clearly.
**Verify:** `go test ./...`; golden regen; `kubectl kustomize manifests/dev-loop/base/`.

### T15 — Apps namespace: make the global default real `opus · M` — *depends: T13, T14*
**Decision (owner-confirmed):** custom app namespaces are a supported feature. Per-app support **already exists** (`add app --namespace`, `app.go:41,120-126,307-317`: explicit flag wins, falls back to `cfg.Namespaces.Apps`, non-default namespaces get a managed doc in `apps/base/namespaces.yaml`). What's broken is the **global default**: `templates/bootstrap/.../namespaces.yaml.tmpl` only ever creates the literal `apps`, while `add app` skips creating a doc for the default namespace "because it's created cluster-side" (`app.go:307-311`) — so `namespaces.apps: myapps` lands workloads in a namespace nothing creates.
**Change:**
1. Thread `AppsNamespace` (from cfg) into `bootstrapValues` and `namespaces.yaml.tmpl` so the bootstrap creates the *configured* default namespace.
2. Audit every consumer of the `namespaces.apps` config key: the flywheel-config ConfigMap producer (fixed by T13), `image-builder-controller.yaml`'s `configMapKeyRef`s / `internal/controller/config.go` (what does the controller do with it — job namespace? verify it flows), `usecmd`/IUA paths, and the `add app` skip logic (now consistent by construction).
3. Add a test: render the bootstrap tree with `namespaces.apps: myapps` and assert the Namespace object + ConfigMap value + any Kustomization `targetNamespace` all say `myapps`.
**Non-goals:** per-app namespace UX (exists); multi-namespace RBAC hardening (defer unless the audit finds a hard break — if so, report, don't expand scope).
**Accept:** setting `namespaces.apps: myapps` in a fresh init produces a bootstrap that creates `myapps` and scaffolds apps into it end-to-end at the render level; default behavior byte-identical (goldens unchanged except where template placeholders replaced literals).
**Verify:** `go test ./...`; golden regen; *e2e-relevant* (flag for a manual scenario-1 run with a custom apps namespace in the PR).

### T16 — `ApplyDevLoop`: stop ignoring the overlay `sonnet · S`
**Evidence:** `converge/helpers.go:32-50` — `up` passes `…/manifests/dev-loop/overlays/local` as `overlayDir`, but the function only uses it to compute the parent dirs, then writes a transient kustomization whose sole resource is `../base`. Overlay content silently applies via the Flux path only — a half-application stacked on the two-apply-paths design.
**Change:** make the transient kustomization reference the overlay instead of base. The temp dir is created directly inside `devLoopRoot` (`manifests/dev-loop/.flywheel-tmp-overlay-*`), so the overlay is reachable as `../overlays/local` (the current `../base` resolves the same way). Confirm the image-rewrite `images:` block and `gitServerMemoryPatch` still apply cleanly on top of the overlay (kustomize applies images/patches after resource resolution — they do, but verify the patch target exists in the overlay render). Update `renderDevLoopKustomization`'s comment. If the overlay is currently empty/passthrough, behavior is identical — state that in the PR.
**Accept:** unit test: drop a marker resource into a fixture overlay and assert `ApplyDevLoop`'s rendered kustomization includes it (use `kubectl kustomize` on the temp tree in-test, or assert the rendered kustomization string references the overlay). Two-apply-paths check: Flux path already applies the overlay — this converges the two paths, no template change needed.
**Verify:** `go test ./internal/cli/converge/...`; `kubectl kustomize` on a constructed temp overlay.

### T17 — e2e recipe: one source of truth `opus · L`
**Evidence:** `e2e-full.yml:24-156` is a near-verbatim copy of `test.yml:76-210` (verified by diff; the header admits it "MIRRORS the per-PR job's known-good setup exactly"); scenarios 2-4 run only in the nightly copy, so drift means silent rot (known). `scripts/e2e.sh` is a third copy that has already drifted (missing: doctor scenario, docker-settle wait `test.yml:123-134`, crictl re-import net `test.yml:169-191` — all added after real CI flakes). Timeouts are 14 scattered per-scenario literals tuned reactively (`scenario-4:48`: "timed out at 180s in CI (converged in ~40s locally)"). Scenarios are order-coupled (scenario-2 asserts scenario-1's leftovers; scenario-5 hard-exits without a prior build job). A red nightly alerts no one.
**Change (in order of value):**
1. Extract the k3d-e2e job into a reusable workflow (`.github/workflows/e2e-recipe.yml`, `workflow_call`) with inputs `scenarios` (string, e.g. `"1 5"` / `"all"`) and `timeout-minutes`. `test.yml` and `e2e-full.yml` become thin callers. Preserve every hardening step verbatim (docker-settle, crictl net, port-squat heal) — this is a move, not a rewrite.
2. Add `TIMEOUT_SCALE` support to `testdata/scenarios/lib.sh` (wrap the wait helpers; default 1, CI callers export 3) and convert the scenario literals to `$(scaled <n>)`.
3. Rewrite `scripts/e2e.sh` as a thin local driver that reuses the same scenario entry points (it can't run workflow YAML — the goal is that all *logic* lives in `lib.sh`/scenario scripts, and e2e.sh + the reusable workflow share only trivial glue). Restore the missing hardening steps to it or document why they're CI-only.
4. Nightly alerting: on failure, `e2e-full` creates/comments a GitHub issue (`gh` step) so red nightlies page someone.
**Non-goals:** decoupling scenario order (2 depends on 1's state by design — document the chain at the top of `run-all.sh` instead; making them self-bootstrapping would multiply runtime).
**Accept:** the recipe exists once; `test.yml` calls it with `scenarios: "1 5"`, `e2e-full.yml` with `all`; a deliberate recipe edit shows up in both jobs' runs; `TIMEOUT_SCALE=3` visible in CI env; failure-issue step tested via a forced-fail branch run or `workflow_dispatch`.
**Verify:** `workflow_dispatch` run of both callers on the PR branch (workflows on a branch are runnable via dispatch if configured — otherwise merge behind a follow-up dispatch test and watch the first nightly).

### T18 — Derive image wiring from `schema.ImageNames` `opus · M`
**Evidence:** adding a fifth controller image today touches ~9 files / ~20 sites: `Dockerfile.<name>`, `.goreleaser.yaml` (4 blocks; commit 5f1c7d7 added +39 lines for one image), `Makefile:19` `IMAGES` + `:58` build loop, `scripts/e2e.sh`, 3 sites each in `test.yml` and `e2e-full.yml`, `dependabot.yml`, `schema.ImageNames` (`schema.go:60`), `manifests/dev-loop/base/`, and the correct one of two template `images:` blocks (`builders-kustomization.yaml.tmpl` lists 3 images, `client-builders-kustomization.yaml.tmpl` the 4th — split explained only in a template comment). On the Go side `bootstrapValues` (`converge/bootstrap.go:59-111`) hand-unrolls exactly four images into 8 named keys + a formatted validation map, while `renderDevLoopKustomization` 40 lines away already loops generically.
**Change:**
1. Make `bootstrapValues` loop over `schema.ImageNames` (generic map of name→{Name,Tag} passed to templates; templates range over it or, if the 3/1 split must stay, drive the split from a small Go-side table with the *reason* encoded — not prose).
2. Agreement test: union of the two templates' `images:` entries == `schema.ImageNames` (render + parse the YAML in-test).
3. T17 reduces the workflow sites to one; `make images` (Makefile loop) becomes the single build recipe the reusable workflow calls (delete inline docker-build steps).
4. What cannot be derived (goreleaser blocks, Dockerfiles, dependabot): write `docs/dev/add-controller-image.md` — a short, complete checklist of the remaining touch points, linked from a comment next to `ImageNames`.
**Accept:** hand-unrolled `GitServerImageName`-style keys deleted; the agreement test fails if a name is added to `ImageNames` without a template entry; checklist doc exists and is accurate (walk it mentally against 5f1c7d7's file list).
**Verify:** `go test ./...`; golden regen if templates changed; `goreleaser release --snapshot --clean` still builds all images.

---

## Phase 2 — structural

### T19 — `up.Run` step table `opus · L` — *depends: T10, T13, T14 (do them first to avoid churn)*
**Evidence:** `up.Run` is 405 lines (`up.go:65-478`); the "15-step" numbering is broken (no steps 4/12, step 8 placeholder, step 6b executes after step 7's setup) and **load-bearing**: numbers appear in other packages' docs and in error strings (`fmt.Errorf("step 11b: %w", …)`, k3d/flux/converge package docs). Fatal-vs-best-effort policy is an ad-hoc call-site choice (11c warns+continues; 11d failure silently disables 11e via `bootstrapOK`; 1-11b abort). `Run` has zero tests.
**Change:** restructure as a table of steps: `type step struct { name string; critical bool; skip func(Options) bool; run func(*upState) error }` executed by a loop that owns the warn-vs-abort policy and step logging. State passed via an explicit `upState` struct (cfg, applier, cacheDir, sha, keep-refs, bootstrapOK becomes explicit state). Replace step numbers with step *names* in every error and cross-package comment (grep `step 1[0-9]` and `step [0-9]` across the repo). Preserve behavior exactly — this is a mechanical restructure; use the review's policy map (which steps warn, which abort, what 11d's failure must disable) as the spec, and encode the 11d→11e dependency explicitly (e.g. prune step's `skip` consults state).
**Accept:** `Run` body ≤ ~80 lines (table + loop); unit test executes the table with stubbed steps asserting ordering, abort-on-critical, warn-and-continue, and the bootstrap-failed→prune-skipped rule; no numeric step references remain (`grep -rn 'step 1' internal/ cmd/` clean of pipeline refs).
**Verify:** `go test ./...`; *e2e-relevant* — request a local scenario-1 run before merge; nightly covers the rest.

### T20 — `add app`: transactional ordering + dedup validation `opus · M`
**Evidence:** `add/app/app.go:79-330` — doc claims a "5-step pipeline"; body numbering runs 1,2,3,4,3,4,5,6,7,7b,8. DNS-label validation repeated 3× (lines 149,184,203-ish). The comment at ~237 claims "we never leave the repo half-scaffolded", but `UpsertWorkspaceRepo` mutates `flywheel.yaml` (line ~283) *before* the renders — a later failure in `render.Tree`/`appendResource` leaves workspace entry, rendered tree, and kustomization in inconsistent combinations with no rollback.
**Change:** reorder so all fallible work (derivation, validation, renders into a staging temp dir, kustomization-edit dry runs) happens before any mutation of the client repo; then commit mutations in a short, unlikely-to-fail tail (move rendered dir into place, apply kustomization edits, `UpsertWorkspaceRepo` **last** — it's the registration). If a tail step still fails, print exactly what was written and how to undo. Extract the repeated DNS validation into one helper. Fix the step-number comments while there (names, not numbers — match T19's convention).
**Accept:** a test that injects a render failure (bad template via `Options.TemplateFS` — the injection seam exists) and asserts `flywheel.yaml` is untouched; existing behavior tests pass unchanged; validation helper has one definition.
**Verify:** `go test ./internal/cli/add/...` (the package's test suite is strong — lean on it).

### T21 — Source-modes owning package `opus · M` — *depends: T10*
**Evidence:** the local-only lifecycle has five implementations: guard in `add/app/app.go:274-280`, flip in `publishapp.go:62-101`, consume in `up/up.go:512-540` (`reconcileWorktrees`), report in `doctor/workspace.go:38-59`, and the app↔worktree↔local-only join inlined a 4th time in `cmd/flywheel/commands.go:306-333` (shell completion — the only copy outside a domain package), plus the bash re-implementation `check-local-only.sh` shipped into client repos. The "exactly one of `url`/`local_only`" invariant is enforced separately in `schema.Validate`, `config/edit.go:264-276`, and the bash yq queries. The join key (`Worktree = basename(spec.url) - .git`) is derived in `worktree.ParseAppGitRepository` and re-derived in bash.
**Change:** create one owning package (e.g. `internal/cli/sourcemode` or extend `internal/cli/worktree`) exposing: the invariant check, the join (declared apps × workspace block × gitrepository specs), and mode predicates/transitions. Migrate the four Go call sites (especially the completion join out of the cobra wiring). The bash script stays (frozen in client repos) — add an agreement test that runs `check-local-only.sh` against a fixture repo and asserts it agrees with the Go join on the same fixtures (both pass/fail sets), so drift is caught in flywheel's CI even though clients can't be updated.
**Accept:** one package owns the join; `commands.go` completion calls it; agreement test exercises bash vs Go on ≥3 fixtures (local-only ok, violation, hand-renamed manifest edge — document the bash script's known blind spot from the review if it can't see renamed files).
**Verify:** `go test ./...`; requires bash 3.2-compatible script (after T05).

### T22 — Applier tests + per-object error aggregation `opus · M`
**Evidence:** `internal/cli/applier` — the SSA engine imported by 9 packages, including destructive paths — has zero `_test.go`. `applyYAML` folds N per-object failures into a single `lastErr` (`applier.go:120-152`): a 30-doc apply failing on docs 3 and 7 reports only doc 7.
**Change:** (1) tests with `k8s.io/client-go/dynamic/fake` covering: multi-doc apply happy path, per-object failure continues + aggregates, `ListUnstructured`/`ListByKindLabeled` selector behavior (locks in T01), `DeleteResource` NotFound tolerance, field-manager set on applies. (2) switch `applyYAML` to `errors.Join` (Go ≥1.20) so all failures surface; check the callers that string-match or count on the error (grep callers in up/converge/usecmd) still behave.
**Non-goals:** portforward.go (needs a live cluster; leave it).
**Accept:** applier ≥70% statement coverage on applier.go; a two-failure apply reports both objects by name.
**Verify:** `go test ./internal/cli/applier/... -cover`.

### T23 — Controllers: one poke helper, reconcile tests, preflight re-probe `opus · L`
**Evidence:** the Flux annotation-poke (`reconcile.fluxcd.io/requestedAt` merge-patch, NotFound-is-OK) is implemented 4× in Go: `pokeImageRepository` (`buildjob_imagescan_controller.go:130-146`), `pokeIUA` (`imagepolicy_iua_controller.go:114`), `pokeGitRepository` (`iua_source_poke_controller.go:120`), `K8sFlux.pokeReconcile` (`selfsync/flux.go:119`); the annotation const is declared 3×. GitRepository is accessed typed in one file but via re-declared unstructured GVKs + stringly field paths in others (`iua_source_poke_controller.go:42`, `selfsync/flux.go:25`) though `sourcev1` is already imported. `preflight.canList` (`preflight.go:24-39`) runs once at startup; RBAC landing seconds later leaves poke controllers disabled until pod restart, Info-logged. Test gaps: no fake-client Reconcile test for the core GitRepository→Job path; `imagepolicy_iua_controller`, `iua_source_poke_controller`, `preflight`, `K8sFlux` have zero tests.
**Change:**
1. One `pokeReconcile(ctx, client, gvk/obj-ref) `helper in `internal/controller` (or a tiny shared package selfsync can import; mind the client types — controller-runtime `client.Client` vs dynamic — pick the seam that serves both or accept two thin adapters over one core).
2. Use typed `sourcev1.GitRepository` where already vendored; delete the duplicate GVK vars for that kind.
3. Preflight: re-probe on a slow ticker (e.g. every 5m) or on RBAC-denied errors, enabling the poke controllers when permissions appear — the manager supports adding runnables; if dynamic controller registration is awkward, the pragmatic fix is: on preflight failure, log at Warn (not Info) with the restart hint AND make the deployment's readiness reflect degraded mode. Keep it minimal — don't rebuild the manager.
4. Tests: fake-client Reconcile tests — GitRepository + build-config → Job created; existing Job → skipped; the mid-loop invalid-secret ordering (document/decide the cross-build coupling at `gitrepository_build_controller.go:231-265`: today build N's bad secret blocks later builds' Jobs — preserve or fix, but write the test either way); poke helper unit tests (annotation set, NotFound tolerated).
**Accept:** one poke implementation in Go; `grep -rn 'requestedAt' --include='*.go'` → helper + naming const only; new tests cover the listed paths; preflight degradation is Warn-level and recoverable (or explicitly readiness-gated).
**Verify:** `go test ./internal/controller/... ./internal/selfsync/...`.

### T24 — One YAML-editing dialect for client files `opus · L` — **risky; tests first**
**Evidence:** three hand-rolled mechanisms edit client-owned YAML: (a) `config/edit.go:80-210` byte-splicing keyed on `yaml.Node` line spans — `maxNodeLine` walks only content-node lines, so foot comments and multi-line scalars (`url: >-`) extend past the computed span → corruption risk; (b) `edit.go:229` `editRoot` re-encodes the whole document and "may normalise blank lines" — and it runs *automatically* via `up`'s port-heal (`SetClusterPort`), reformatting a user's committed file; (c) `add/app/app.go:456-570` raw line-scanning of kustomizations (`trim == "resources:"` misses `resources:  # comment`; namespace detection is exact-string matching). This file class has already eaten user data once (the removed `update` command clobbering `resources:` lists).
**Change:** consolidate on the comment-preserving `yaml.Node` approach in one package (`internal/cli/yamledit`): node-level insert/append/set that re-serializes only via the node tree (no byte splicing, no whole-doc re-encode where avoidable). **Order of work:** first write a brutal golden-test corpus (foot comments, inline comments on `resources:`, multi-line scalars, 4-space indent, CRLF?, empty lists, duplicate keys) against the *current* three implementations to document today's behavior; then implement the new editor to pass the corpus; then migrate `appendResource`/`ensureNamespace` and `editWorkspaceRepos`/`editRoot`. If full unification stalls, the minimum viable cut is: fix (a)'s `maxNodeLine` bug + migrate (c) to yaml.Node; leave `editRoot` documented.
**Accept:** the corpus passes; `add app` against a kustomization with `resources:  # comment` and 4-space indent works; `SetClusterPort` no longer reorders/reformats untouched sections (assert byte-identical outside the edited scalar).
**Verify:** `go test ./internal/cli/config/... ./internal/cli/add/...` with the new corpus.

### T25 — Doctor severity levels `sonnet · M`
**Evidence:** the `Check` registry (`doctor/doctor.go:16-24`) is sound, but pass/fail only: `workspace.go:18-21` admits "every actionable finding surfaces as one informational failure", and `commands.go:56-70` exits non-zero on any failure — so a missing local-only sibling (documented as "never gates `up`") is indistinguishable from docker-down.
**Change:** add `Severity` (error/warn/info) to check results (not to `Check` — a check can return findings of mixed severity; look at how `workspace.go` builds findings and pick the smaller diff). Renderer prints them distinctly; exit code non-zero only on `error`. Reclassify: host-prereq failures = error; workspace/local-only advisories = warn.
**Accept:** `flywheel doctor` with only advisory findings exits 0 and prints them as warnings; docker-down still exits 1; `scenario-doctor.sh` updated if it asserts exit codes.
**Verify:** `go test ./internal/cli/doctor/... ./cmd/...`; run `testdata/scenarios/scenario-doctor.sh`.

### T26 — Embed/golden completeness guard `sonnet · S`
**Evidence:** golden tests render from `os.DirFS(skeletonDir)`, not the embedded FS (`initcmd/init_test.go:60`), so `//go:embed` exclusions can't fail them; the compensating check hardcodes exactly two files (`embedded_skeleton_test.go:16`: `.gitignore.tmpl`, `.sops.yaml.tmpl`) — a third dotfile template would be silently missing from release binaries.
**Change:** replace the two-file list with a dynamic comparison: walk `templates/client-skeleton` on disk and assert every file exists in `embeddedSkeleton()` (and same for `manifests/` vs the manifests embed if applicable — check `embed.go`'s patterns). Alternatively (better if cheap): point one golden test at the embedded FS directly.
**Accept:** adding a new dotfile template with a missing embed pattern fails `go test` at the completeness check.
**Verify:** `go test ./... `; temporarily add `.fake.tmpl` + no embed change → test fails → remove.

### T27 — Completion paths: single source `sonnet · S`
**Evidence:** the three completion destinations (zsh/bash/fish paths) are hardcoded in `scripts/install-completions.sh:21-43`, `install.sh:199-203`, and `uninstall.sh:114-116`, with sync-by-comment (`uninstall.sh:107`).
**Change:** make `scripts/install-completions.sh` the single owner: give it `--print-paths` (machine-readable) and have `uninstall.sh` consume it when available, falling back to the static list only for old installs; `install.sh` already can't source it at install time (it inlines by design, `install.sh:188-190`) — so at minimum add a CI check (tiny shell test) asserting the three files' path lists are identical, which turns silent drift into a test failure.
**Accept:** one authoritative list + a drift check in CI (`test.yml` cheap job).
**Verify:** shellcheck; the drift check fails when one path is edited alone.

### T28 — imagepin: injectable seams + tag-picker dedup `sonnet · M`
**Evidence:** `internal/cli/imagepin/imagepin.go` (449 lines): tests stub via 4 mutable package vars (`daemonArch`, `remoteImage`, `remoteWrite`, `inLocalDocker`) with manual save/restore (`imagepin_test.go:144-146`) — forbids `t.Parallel`, leaks on forgotten restore. `remoteTag` (:324) and `registryTag` (:345) are near-duplicates differing only in digest source. `DefaultRef` hardcodes `ghcr.io/cobr-io/%s:%s` (:73); `inClusterRegistryPort` (:67) admits duplicating the controller's constant.
**Change:** group the four func vars into a single deps struct passed to (or embedded in) the entry points, defaulting to real implementations — tests construct their own (enables `t.Parallel`, no globals). Merge the two tag pickers with a digest-source parameter. Move `ghcr.io/cobr-io` + the registry port to `internal/naming` (T10) so the controller duplication note can be resolved.
**Accept:** zero mutable package vars in imagepin; tests run with `t.Parallel()`; one tag-picker function.
**Verify:** `go test ./internal/cli/imagepin/... -race`.

### T29 — Shared template render context `opus · M` — *depends: T14, T18 (they reshape the values)*
**Evidence:** the render context is four disconnected stringly-typed builders: `initcmd/init.go:426`, `converge/bootstrap.go:59` (`bootstrapValues`), `add/app/app.go:336` (`buildValues`), + an anonymous struct in `render/render_test.go:63`. Key names already drift (`"Domain"` in init/bootstrap vs `"LocalDomain"` in add-app). `missingkey=error` (`render/render.go:86`) means a mismatch fails only when that command renders that template.
**Change:** one typed context struct (or a small set: `SkeletonContext`, `BootstrapContext`, `AppContext` sharing an embedded common core) in the render (or schema) package, built from `*schema.File` in one constructor; commands add their extras via typed fields, and `render.Tree` accepts the struct (template `{{ .Field }}` works on structs — no map needed). Reconcile the `Domain`/`LocalDomain` drift (grep templates for both; pick one, alias during migration if templates in client repos reference the old name — they don't; templates render at init time, so a clean rename inside this repo is safe).
**Also:** while here, honor or delete the dead `FluxIntervalLocal` knob (documented as "baked into rendered flywheel.yaml" but referenced by zero skeleton templates — `flywheel.yaml.tmpl` hardcodes `interval_local: 10s`); wire it or remove the option.
**Accept:** one constructor owns cfg→context; `render_test` uses the real struct; adding a field is one struct edit + template use; goldens unchanged (pure refactor) except where the dead-knob fix lands.
**Verify:** `go test ./...`; golden regen.

### T30 — git-deploy-controller: health probes `sonnet · S`
**Evidence:** `cmd/git-deploy-controller/main.go` is a hand-rolled ticker loop with `log.Printf`, no healthz/readyz, and its manifest (`manifests/dev-loop/base/git-deploy-controller.yaml`) has no probes — the other controller has the full controller-runtime probe set.
**Change:** minimal hygiene, not a rewrite: add an HTTP healthz endpoint (liveness = process up; readiness = last loop iteration completed within N ticks) and wire probes into the manifest. **Two-apply-paths check:** manifest change must be identical on both paths (it's in `dev-loop/base`, rewritten by both rewriters — image refs only, so a probe addition is single-sided; confirm no bootstrap-template patch targets this Deployment).
**Non-goals:** controller-runtime rewrite, metrics, structured logging — see Deferred.
**Accept:** `kubectl kustomize manifests/dev-loop/base/` shows probes; loop-stall (test with a stubbed slow iteration) turns readiness false.
**Verify:** `go test ./...`; kustomize render.

---

## Deferred (explicitly out of scope — revisit only with a concrete driver)

- **git-deploy-controller → controller-runtime rewrite** (watch-driven instead of 2s polling). Real improvement, but a rewrite of working code; T30 covers the operational gap. Driver to revisit: CPU/API-load complaints or a third controller idiom appearing.
- **Go port of the per-app sync.sh sidecar.** Known scaling lever (see git-sync idle-CPU history — a Go controller is the answer if app count grows); T06 shrinks the bash surface in the meantime.
- **Multi-env parameterization** ("local" as a structural directory name across the embed path, four Flux `spec.path`s, and Go joins). The single-env design is deliberate; parameterizing now is speculative. The promotion guide owns this story.
- **Doctor check parallelism / registry plugins, imagepin non-ghcr registries** — no current need.

## Suggested execution order

Phase 0 in any order, in parallel (all Sonnet, no conflicts except T05/T07 both touch golden files — land sequentially).
Then: **T10 → (T11, T12, T13 in parallel) → T14 → T15**, with **T16, T17, T18** free-floating in phase 1 (T18 after T17 if both touch workflows).
Phase 2 anytime after its dependencies; **T24 last among edits to client files** (highest risk, wants T20's staging groundwork), **T19 after the phase-1 config/namespace tasks** to avoid rebasing the step bodies twice.
