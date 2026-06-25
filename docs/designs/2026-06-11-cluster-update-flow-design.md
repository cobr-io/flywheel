# `flywheel update` — guided cluster + template update flow

**Status:** implemented
**Date:** 2026-06-11
**Author:** matthijs (with collaboration from Claude)

## Problem

There is no clean, supported way to apply a newer flywheel to an existing
cluster. The current reality, uncovered while shipping the git-server
stale-lock fix (issue #9):

1. **Images don't roll on rebuild.** Dogfood images use a mutable
   `:dogfood` tag. `make k3d-import` imports new content under the same
   tag, but because the tag is unchanged, neither `flywheel up` (an SSA
   no-op on an unchanged Deployment) nor Flux rolls the workload. A
   rebuilt image never reaches the running pod without a manual
   `kubectl delete pod`. We hit exactly this: the fixed `git-server`
   image sat on the host while the cluster kept running the old one.
2. **Manifest cache is silently stale.** `flywheel up` keys the embedded
   asset cache on `flywheel.version` (`embedcache.go:138`). Ship a newer
   binary without bumping the version and `up` reuses the old extracted
   manifests — the new content is silently ignored.
3. **Reconcile is additive-only.** `up` step 12 (orphan deletes) is a
   stub (`up.go:121`); removed/renamed resources linger forever.
4. **No upgrade command.** "Upgrade" today means: bump `flywheel.version`,
   run a newer binary's `up`, then manually recreate pods — with no
   guidance and several silent traps.
5. **Scaffolded templates drift.** `flywheel init` renders
   `templates/client-skeleton/*` and `docs/guides/*` into the client's
   gitops repo. A newer binary embeds newer templates, but nothing
   reconciles the user's (possibly hand-edited) committed copies. The
   `.flywheel-state.yaml` written at init was designed for this 3-way
   merge (`state.go:11`) but no consumer exists yet.

We want a single command, `flywheel update`, that **guides the user
end-to-end** through converging both the running cluster and the
client-repo templates to whatever the running binary embeds.

## Goals / non-goals

**Goals**

- One command, `flywheel update`, that converges (A) the cluster runtime
  and (B) the client repo's scaffolded templates.
- **Guided interactive wizard by default**; flags are the non-interactive
  (CI / scripting) contract, not the primary interface.
- Binary-is-the-version: running a newer binary is the trigger; no manual
  `flywheel.version` bump (kills trap #2).
- The two layers are independently runnable and independently fail-safe.
- Designed-for ghcr (released-image) delivery, even though v1 executes the
  dogfood (local-build + k3d) path.

**Non-goals (v1)**

- Cross-version bespoke migration scripts (assume adjacent-version, render-
  driven convergence).
- Multi-cluster fan-out; remote (non-k3d) cluster apply.
- Auto-committing the user's gitops repo (we stage; the user commits).

## Decisions (locked during design)

| Decision | Choice |
|---|---|
| Surface | Dedicated `flywheel update` (not folded into `up`), plan → confirm → apply |
| v1 scope | Full convergence **including prune** (real destructive-set detection) |
| Image identity | **Content-addressed tags** (`dogfood-<gitsha>` / `:<version>`) — k8s rolls automatically |
| Build boundary | `flywheel update` **orchestrates the build** (shells out to `docker build`) |
| Template scope | Layer B (client-repo 3-way merge) included in the same command |
| Conflict UX | Real **3-way merge**; prefer `git mergetool`, fall back to `$EDITOR` + `<<<<`/`====`/`>>>>` markers |
| Trigger/version | **Binary is the version**; `update` rewrites `flywheel.version` + state |
| Composition | **Approach 2** — independent phases, **Layer A first**, each fail-safe |
| State baseline | **Split** into a cluster baseline and a template baseline, advanced independently |
| Interaction | **Guided wizard by default**; flags = non-interactive overrides |

## Two layers

| | **Layer A — cluster runtime** | **Layer B — client-repo templates** |
|---|---|---|
| Owns | Images, embedded machinery manifests (mirror), Flux state, orphans | Scaffolded `.tmpl` output committed in the user's repo + `docs/guides` |
| Owner | Machine (k8s/Flux) | User (edits these files) |
| Delivery | content-addressed tags → auto-roll; SHA-keyed mirror push; prune | 3-way merge vs `.flywheel-state.yaml` baseline |
| Risk | Low — idempotent, converges | High — conflicts, user edits, partial merge in a git repo |

`templates/bootstrap/*` are re-rendered fresh by `up`/`update` every run
(`up.go:249`), never committed — they self-update with no merge and belong
to neither layer's merge logic.

## State model — extend `.flywheel-state.yaml`

Today: `FlywheelSHA`, `Answers`, `Files{relpath→sha256}`. We split the
single SHA into two independently-advanced baselines and record what the
cluster last received:

```yaml
flywheel_sha: 9f4e1a2        # DEPRECATED flat field; migrate-read only
cluster:
  converged_sha: 9f4e1a2     # Layer A: embedded-asset SHA last pushed + images rolled
  image_tags:                # content tag last delivered per image (drift compare)
    git-server: dogfood-9f4e1a2
templates:
  baseline_sha: 3058b28      # Layer B: SHA whose render is the 3-way MERGE BASE
  answers: {…}               # committed flywheel.yaml snapshot at baseline
  files: {relpath: sha256}   # rendered-content hashes at baseline
```

- `cluster.converged_sha` advances when Layer A finishes.
- `templates.baseline_sha` advances **only** on full clean Layer B
  finalization — a deferred/conflicted Layer B keeps its old baseline so
  the merge base stays valid.
- `image_tags` enables tag-comparison drift detection (no digest plumbing).
- A flat `flywheel_sha` from an older state file is migrate-read into both
  `cluster.converged_sha` and `templates.baseline_sha` on first `update`.

## Command surface

```
flywheel update                  # guided: plan → Layer A → Layer B, interactive
flywheel update --cluster-only   # Layer A only (urgent fixes; what #9 needed)
flywheel update --templates-only # Layer B only
flywheel update --continue       # resume an in-progress template merge
flywheel update --abort          # discard in-progress merge, restore baseline
flywheel update --dry-run        # plan only, no mutation
flywheel update --prune          # non-interactive: pre-approve deletions
flywheel update --no-prune       # non-interactive: pre-decline deletions
flywheel update --no-edit        # leave conflict markers instead of launching editor
flywheel update -y/--yes         # assume yes for non-destructive steps
flywheel update --commit         # auto-commit staged template changes (default: stage only)
```

## Interaction model — guided wizard

On a TTY, `flywheel update` walks the user end-to-end:

1. **Plan** — load + merge `flywheel.yaml(.local)`; read state; detect
   target = running binary's embedded SHA/version; print the combined
   Layer A + B diff; confirm to proceed.
2. **Layer A converge** — build/import/roll/push with progress. For each
   **orphan**, prompt inline: `Delete ConfigMap/old-feature-flags?
   [y/N/a=all/q=quit]`. CRD/PVC are **never** offered (report-only).
3. **Layer B merge** — clean-tree precondition; run 3-way merges; then for
   each conflicted file in sequence prefer `git mergetool` (3-pane), else
   open `$VISUAL`/`$EDITOR` on the markered file. On close, re-scan for
   markers: unresolved → `[e=reopen / s=skip for now / a=abort]`; clean →
   next. Clean upstream changes / additions apply with a per-step prompt;
   prunes use the same prompt as Layer A.
4. **Finalize** — once all conflicts are clean: advance
   `templates.baseline_sha`, refresh `answers` + `files`, stage the
   changes, show a `git status`-style summary, prompt `Commit now? [y/N]`
   (or `--commit`).
5. **Resume** — re-invoking with a merge in progress drops straight back
   into the editor loop.

**Non-interactive contract (no TTY / CI):** no prompts or editors.
Destructive decisions must be pre-answered (`--prune`/`--no-prune`),
`-y` assumes yes for non-destructive steps, `--no-edit` leaves markers.
A Layer B conflict writes markers, records in-progress state, and exits
non-zero with guidance ("run interactively, or resolve and
`--continue`"). With an unanswered destructive decision and no TTY,
`update` stops rather than guessing — CI stays deterministic.

## Layer A — cluster convergence

**Content-addressed image tags (root-cause fix for trap #1).** Build tags
become `flywheel-dev/<name>:dogfood-<gitsha>` (dogfood) and
`ghcr.io/cobr-io/<name>:<version>` (released). Both immutable ⇒ the
Deployment ref changes when content changes ⇒ k8s rolls on its own; no
manual pod recreate. `imagepin.Resolve`/`DefaultRef` extend to take a
content tag; the git SHA is the binary's embedded SHA (coherent with the
mirror push). Drift = desired tag ≠ `state.cluster.image_tags[name]`.

**`update` orchestrates the build.** Per drifted image: `docker build -t
flywheel-dev/<name>:dogfood-<sha> -f Dockerfile.<name> .` → `k3d image
import` → patch the resolved ref → Deployment rolls. The build/import step
is a pluggable **image source** (`EnsureImage(name, contentTag) → ref`):
`localBuild` (v1) and `registryRef` (ghcr, later). Everything downstream
is source-agnostic.

**Manifest convergence.** Like `up` step 3/11c but unconditional on a
newer embedded SHA, and **SHA-keyed** (`~/.cache/flywheel/<binary-sha>/`,
not version-keyed — kills trap #2): re-extract embedded assets, push to
the `flywheel.git` mirror, Flux reconciles. `cluster.converged_sha`
advances on success.

**Prune / destructive-set detection (trap #3), gated.** Make the v0.1.1
stub real, scoped to Layer A. Ownership label
(`app.kubernetes.io/managed-by: flywheel` + asset marker); orphans =
labelled resources present in-cluster but absent from the new rendered
set. **Tiering:** CRDs and PVCs are never auto-deleted — only reported,
even under `--prune`/`y=all` (prevents data loss, e.g. the git-server
PVC). Everything else is deletable on approval.

## Layer B — template 3-way merge

**Candidates** = exactly the files in `state.templates.files`. User-created
files (their own apps) aren't in that manifest and are never touched.

**Preconditions:** clean working tree (so writes are distinguishable and
`--abort` restores cleanly); no merge already in progress unless
`--continue`/`--abort`.

**Per candidate**, render three versions and classify:

- **base** = render from `templates.baseline_sha` with `templates.answers`.
- **ours** = file currently on disk (may carry user edits).
- **theirs** = render from the running binary's templates with the current
  committed `flywheel.yaml` answers.

| base→theirs | ours vs base | Action |
|---|---|---|
| unchanged | (any) | skip |
| changed | ours == base | apply theirs |
| unchanged | ours ≠ base | keep ours |
| changed | ours ≠ base | 3-way merge; overlaps → conflict markers |

Merge engine: `git merge-file` / go-git diff3.

**Per-file policy overrides:**

- `flywheel.yaml` — **answer-generated**, never text-merged. Updates to its
  template (new keys/defaults) are applied by re-rendering from merged
  answers, preserving user values.
- New upstream files (in `theirs`, not in `state.files`) → **added**.
- Upstream-removed files (in `state.files`, not in `theirs`) → **delete,
  gated** like Layer A; reported otherwise.

**Resumability (rebase-like):** on conflict, write markers, persist an
in-progress record (files merged/conflicted, target SHA, answers used),
exit non-zero (interactive: stay in the editor loop). `--continue`
verifies no markers remain and finalizes. `--abort` restores all
candidates to pre-merge state and clears the record.

**Finalization (only when fully clean):** rewrite `state.templates`
(`baseline_sha` → running SHA; refresh `answers` + `files`). This is the
**only** path that advances the template baseline. Stage changes; the user
commits (or `--commit`).

## End-to-end flow (bare `flywheel update`)

```
1. Load + merge flywheel.yaml(.local); read .flywheel-state.yaml.
2. Detect target = running binary's embedded SHA / version.
3. Resume check: in-progress Layer B merge → require --continue/--abort.
4. Compute combined plan (Layer A drift + Layer B classification). Print.
5. Confirm (—dry-run exits here; -y for non-interactive). Deletions gated.
6. ── Layer A (skipped if --templates-only) ──
   build→import drifted images; push manifests; apply; prune (gated).
   On success: advance cluster.{converged_sha,image_tags}; write flywheel.version.
7. ── Layer B (skipped if --cluster-only) ──
   precondition: clean tree. 3-way merge candidates.
   - clean    → finalize: advance templates.*, stage changes.
   - conflict → mergetool/$EDITOR loop (interactive) or markers + exit (CI).
8. Summary: what rolled, what to commit, any conflicts outstanding.
```

Layer A runs **first** (Approach 2): the safe, idempotent, machine-owned
half always completes; the risky, resumable, user-file half runs last so a
conflict never leaves the cluster half-done.

## Error handling

- **Layer A partial failure** (an image build fails): abort before the
  mirror push for that asset set; `cluster.converged_sha` does not advance;
  re-runnable (idempotent). Already-rolled images stay (forward-only).
- **Layer B conflict:** expected, resumable — not a hard error. Exit code
  distinguishes "cluster converged, templates need resolution" from a real
  failure.
- **Dirty tree at Layer B:** Layer A still ran (value delivered); Layer B
  refuses with guidance ("commit/stash, then `update --templates-only`").
- **Interrupted mid-merge:** in-progress record + on-disk markers ⇒ next
  invocation routes to `--continue`/`--abort` (interactive: resumes loop).

## ghcr / future (designed-for, not built in v1)

The only differing Layer A seam is the image source
(`EnsureImage(name, contentTag) → ref`): `localBuild` (docker build + k3d
import, v1) vs `registryRef` (assert published; pull-on-roll, later).
Layer B, the state model, the merge engine, plan/confirm, and prune are
source-agnostic and unchanged. Remote (non-k3d) apply is out of scope but
not foreclosed.

## Testing strategy

- **Unit:** state migration (flat `flywheel_sha` → split); image drift
  classifier; the four-case 3-way classification table (golden fixtures);
  per-file policy (`flywheel.yaml` re-render preserves user values); prune
  tiering (CRD/PVC never deleted).
- **Integration:** `update --dry-run` plan golden output; Layer B conflict
  → markers → `--continue` → finalized state; `--abort` restores baseline;
  idempotent re-run is a no-op; non-TTY conflict path exits non-zero with
  guidance.
- **e2e (k3d):** the issue #9 scenario — rebuild `git-server` with a
  content change, `update --cluster-only`, assert the running pod carries
  the new content **without** a manual pod delete; assert no stale locks;
  assert `cluster.converged_sha` advanced.
- **Regression guard (trap #2):** newer binary + unchanged
  `flywheel.version` still converges (SHA-keyed cache).

## Open questions

1. **Merge base render.** Reconstructing `base` needs the baseline binary's
   templates. Options: (a) cache the rendered base alongside state at each
   finalize; (b) re-render from baseline answers using *current* templates
   (wrong if templates changed). (a) is the likely answer — confirm in the
   implementation plan.
2. **`flywheel.version` for dogfood** (no real release tag) —
   `v0.0.0-dev+<sha>`? Affects `DefaultRef` and plan display.
3. **`--continue` cluster re-validation** — re-run Layer A on continue, or
   trust the earlier converge?
4. **`git mergetool` availability detection** — how to robustly detect a
   configured, working mergetool before handing off (vs falling back to
   `$EDITOR` + markers).
