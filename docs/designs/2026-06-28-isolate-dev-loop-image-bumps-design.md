# Isolate ephemeral local image bumps onto a deploy ref

**Status:** proposed
**Date:** 2026-06-28
**Author:** matthijs (with collaboration from Claude)

## Problem

The local dev loop's Flux Image Update Automation (IUA, `flywheel-self`) commits
`chore: bump images` commits that rewrite the `# {"$imagepolicy": ...}` setter
line in `apps/.../deployment.yaml`. It commits to **whatever branch the
`flux-system` GitRepository tracks**, which today is **the branch the
developer's worktree is checked out on**: `up` renders both `spec.ref.branch`
and the sync's `DEFAULT_BRANCH` from `converge.CurrentBranch()`
(`internal/cli/converge/bootstrap.go:127`), and `git-auto-sync-self` then
rebases those bump commits **back into the host worktree**
(`scripts/git-auto-sync/sync.sh:349-449`).

The bump tags are **ephemeral, local-cluster-only** — `<unix-ts>-<sha>` in the
per-client k3d registry. They are machine-generated, regenerated on every build,
and meaningless on any other machine. Yet they land on — and are rebased into —
the developer's working branch.

For a single developer this is "merely" noise on `main`. In the **team /
local-first workflow flywheel is meant to support** it actively breaks the goal:

- Integration happens on `main`; engineers work on their own feature branches off
  `main`, **local-first but pushable**, so teammates can pull / validate /
  contribute.
- When the IUA commits bumps onto an engineer's feature branch and they push it,
  the branch carries commits pinning images that exist **only in their local
  registry**. A teammate who pulls the branch gets a manifest referencing
  unpullable tags → transient `ImagePullBackOff` until their own loop rebuilds
  and re-bumps, plus junk commits in the shared history.

The requirement that follows: **ephemeral local image bumps must never live on
any branch the developer pushes.**

### Why not the obvious alternatives

- **A single "blessed" dev branch** — switch the worktree onto a fixed `dev`
  branch and deploy from there (the interim guard built but not committed; see
  [Migration](#migration-plan)). Wrong shape for a team: it implies everyone
  shares one development branch, and the bumps still pollute *that* branch.
  Superseded by this design.
- **Refuse on `main`, use your own feature branch.** Fixes the *shared-branch*
  problem but not the *pollution* one — bumps still land on the feature branch you
  push, breaking the teammate-pull case above.
- **Eliminate the git bump** — a controller patches the live Deployment image
  in-cluster, nothing committed. Cleanest *locally*, but **breaks production
  parity**, a core flywheel goal: prod's rollout path *is* git-commit-driven IUA,
  so bypassing git locally would exercise a different mechanism than prod runs.
  Rejected for that reason; recorded as a standing constraint.

## Guiding principle: apply prod's separation locally

Prod already separates two roles that the local loop currently conflates onto a
single branch:

| Role | Prod | Local today | Local (this design) |
|---|---|---|---|
| Cluster desired-state ref the IUA bumps | gitops repo mainline | the dev's worktree branch | **DEPLOY** (a `flywheel/local-deploy` branch) |
| Where humans author changes | feature branches → PR → main | the same worktree branch | **AUTHORED** (the worktree branch) |

In prod nobody develops on the branch the IUA writes; CI / promotion does. This
design gives the local loop the same separation: the IUA writes a dedicated
**DEPLOY** ref, and the developer authors on their own branch, which the IUA
never touches. Parity is preserved — it is the *same*
`ImageRepository → ImagePolicy → IUA → git commit → GitRepository → Kustomization → rollout`
machinery, just aimed at a side ref.

## Goals / non-goals

**Goals**

- Image bumps never reach any branch the developer pushes.
- Developer works on their own branch (any name, including `main`); no forced
  "blessed" branch.
- A teammate who pulls a developer's branch gets a portable manifest (their own
  loop bumps it locally).
- Preserve production parity: keep the git-commit-driven IUA path.
- Remove the interim dev-branch guard — no net-new "switch your worktree" magic.

**Non-goals**

- Changing the prod promotion story (separate, deferred design).
- A multi-developer *shared* cluster (each developer still has their own k3d).
- Preserving bump *history* across authored-branch advances — bumps are
  ephemeral; we keep at most a thin current layer.

## Approach

### The two refs

Both are branches of the **gitops repo** — the repo `flywheel up` runs in, holding
`apps/` / `infra/` / manifests (*not* the separate app-code repos that feed image
builds). Each branch can exist in two physical places: the developer's **host
worktree** (their checkout) and the **in-cluster bare repo** the git-server serves to
Flux.

- **AUTHORED** — the developer's **working branch**: where *they* commit their own
  changes (manifest edits, new apps, infra tweaks) and which they push to the shared
  remote for teammates. Whatever branch they choose (`feat-x`, `main`, …) — the design
  imposes no name. "Authored" = human commits, as opposed to machine image bumps. It
  lives in the worktree (where they author) and is mirrored push-only to the bare repo
  so Flux can read it. It is **clean of bumps** and pushable.
- **DEPLOY** — `flywheel/local-deploy`, a **local-only branch in the in-cluster bare
  repo** = `AUTHORED + bump commits`. Flux tracks it, the IUA writes it, and it is
  never pushed to the developer's upstream.

```
AUTHORED  (worktree, pushable):  A───A'              ← authored here; no bumps
                                      \
DEPLOY (flywheel/local-deploy):  A───A'──bump         ← Flux tracks; IUA bumps; never pushed
```

DEPLOY lives **only in the in-cluster bare repo**, and the worktree never fetches or
checks it out — so it never appears in the developer's `git branch` and never
reaches their upstream `origin`. *That* separation, not a clever ref namespace, is
what keeps bumps off shared history. (A non-`refs/heads/` namespace would have made
it structural, but source-controller can't fetch one — see
[Feasibility](#feasibility-resolved-2026-06-28).)

### Data flow

1. Developer commits a manifest change on AUTHORED (worktree).
2. `git-auto-sync-self` pushes AUTHORED → bare repo — **push-only**; nothing comes
   back, so the worktree stays clean.
3. The sync maintains `DEPLOY = AUTHORED + bumps` in the bare repo.
4. Flux tracks DEPLOY, reconciles, deploys.
5. A new image is built → ImagePolicy advances → **the IUA commits a bump to
   DEPLOY** (Flux's GitRepository tracks DEPLOY, and the IUA writes whatever it
   tracks — unchanged from today).
6. The bump lives on DEPLOY. AUTHORED never sees it.

### Component changes

**1. Flux `flux-system` GitRepository** (`templates/bootstrap/clusters/local/flux-system/self-source.yaml.tmpl`)
- `spec.ref.branch: {{ .Branch }}` → `spec.ref.branch: flywheel/local-deploy`
  (constant — no longer the per-developer branch). Plain `ref.branch` (not
  `ref.name`) → **no** dependency on the Flux version's arbitrary-ref support: the
  smoke test ([Feasibility](#feasibility-resolved-2026-06-28)) showed the
  `ref.name` / non-head-ref form **fails to fetch** on the pinned Flux.

**2. IUA `flywheel-self`** (`manifests/dev-loop/base/image-update-automation.yaml`)
- **Unchanged.** It has no `push.branch`, so it writes whatever the GitRepository
  tracks — now DEPLOY. This is the keystone: the parity-critical component needs
  no change.

**3. `git-auto-sync-self` / `sync.sh`** — the bulk of the work, in two parts:
- *Remove* the rebase-bumps-into-worktree path (`sync.sh:349-449`,
  fast-forward / rebase-worktree). With bumps confined to DEPLOY, bare-AUTHORED
  only ever receives from the worktree, so worktree→bare is a clean push (no
  force-with-lease dance, no divergence).
- *Add* DEPLOY maintenance — keep `DEPLOY = AUTHORED + bumps` (algorithm below).
- *Seed* the DEPLOY branch by **push** on first reconcile: `flywheel/local-deploy`
  does not exist in the developer's source worktree (that is the point), so the
  reconciler creates it in the bare repo with an explicit push (receive-pack is
  enabled in-cluster). It is never fetched back into the worktree.

**4. `flywheel use <branch>`** (`internal/cli/usecmd/usecmd.go`)
- Flux's ref is now constant (DEPLOY), so `use` no longer repoints Flux. It
  instead sets the **configured AUTHORED branch** the sync feeds DEPLOY from
  (reusing the `flywheel.cobr.io/deploy-branch` annotation mechanism). The
  `AUTO_FOLLOW_BRANCH=false` safety model (issue #17 — don't follow transient
  worktree checkouts during a rebase) carries over: the sync tracks the
  *configured* branch, not the live `HEAD`.
- **Default AUTHORED branch = the integration branch** (`git.integration_branch`,
  default `main`), read from the `flywheel-config` ConfigMap — *not* the branch
  `up` was run on. So a deliberate `flywheel use <feature-branch>` is required to
  deploy a feature branch; with none set, DEPLOY tracks the integration branch.
  (This is a behaviour change from the old sync, which deployed whatever branch
  the worktree was on at `up` time — consistent with the deliberate-selection,
  don't-auto-follow model.)

**5. The interim dev-branch guard** — deleted (see [Migration](#migration-plan)).

### DEPLOY maintenance algorithm

DEPLOY must equal `AUTHORED + bumps` at all times. Three events change it:

- **The IUA adds a bump** (image changed): it commits directly onto DEPLOY.
  Nothing for the sync to do.
- **AUTHORED advances on the same branch** (developer commits): DEPLOY must move
  from `A + bumps` to `A' + bumps` — rebase the bump layer forward (below).
- **`flywheel use` switches to a different branch**: DEPLOY is **reset** to the
  new branch's tip, discarding the old branch's bumps (they were built from a
  different branch's code) — the IUA then re-bumps for the new branch. A
  rebase-forward would *wrongly* replay the old branch's authored commits onto the
  new one, so a switch is a reset, not a rebase. (`deploybranch.Maintainer.ResetToAuthored`;
  the loop detects the switch by comparing the resolved AUTHORED branch to the one
  DEPLOY was last built from.)

To move it, **suspend the IUA, rebase the bump layer forward, resume** — reusing
the suspend/resume pattern the sync already has for branch switches (`IUA_NAME` in
`self-git-auto-sync.yaml.tmpl`; `sync.sh:165-186`):

```
patch IUA flywheel-self  spec.suspend=true
git fetch bare  AUTHORED, DEPLOY
git rebase --onto <AUTHORED'> <old AUTHORED base> DEPLOY   # carry bumps onto A'
git push --force-with-lease bare DEPLOY
patch IUA flywheel-self  spec.suspend=false
trigger reconcile (poke GitRepository + the client-apps Kustomization)
```

Suspending during the rebuild removes the sync↔IUA write race on DEPLOY.

- **Rebase-forward** keeps a *real* image on DEPLOY throughout — no broken window.
  It conflicts only when the developer's commit lands in the **same diff hunk** as
  the machine-managed `$imagepolicy` setter line — the image line itself or an
  adjacent line (e.g. editing the container's `name` / `ports` / `env`). Edits
  elsewhere in the manifest (`replicas`, labels, other containers) rebase cleanly.
  Both paths confirmed hermetically (see [Feasibility](#feasibility-resolved-2026-06-28)).
- **Fallback on conflict:** reset DEPLOY to `AUTHORED'` and let the IUA re-bump
  (the poke controller makes this near-instant). Cost: a brief window where DEPLOY
  references `:0-placeholder` → transient `ImagePullBackOff`. Log it.

## Feasibility (resolved 2026-06-28)

The Flux-version and git-server halves are both confirmed — **but a cluster smoke
test rejected the non-head-ref form.** DEPLOY must be a plain `refs/heads/` branch
(`flywheel/local-deploy`). Details below.

**Flux supports `spec.ref.name`.** The pinned install is Flux **v2.8.7** with
**source-controller v1.8.4**, embedded as a pristine manifest
(`internal/cli/flux/flux.go:25-30`, `const Version = "v2.8.7"`; applied via SSA, no
`flux install --version` shell-out). The bundled GitRepository CRD defines
`spec.ref.name` as an arbitrary valid-Git-reference field
(`internal/cli/flux/install.yaml:1042-1048`), alongside `branch` / `tag` / `semver`
/ `commit`. Flywheel's own Go API types (`source-controller/api v1.9.0`, `go.mod`)
carry it too, so the CLI can set/patch it.

**The git-server advertises and serves custom refs.** It runs git smart-HTTP via
nginx → fcgiwrap → `git-http-backend` with `GIT_HTTP_EXPORT_ALL=1`, `http.receivepack
true`, and **no** `transfer.hideRefs` / `uploadpack.*` restriction
(`scripts/git-server/entrypoint.sh`, `manifests/dev-loop/base/git-server.yaml` —
image `ghcr.io/cobr-io/git-server:v0.1.0`). upload-pack advertises every ref under
`refs/*`; the GitRepository reaches it over plain in-cluster `http://` (no TLS/auth)
— `self-source.yaml.tmpl`.

**Hermetic git verification** (git 2.54.0) confirmed the mechanics end-to-end:
- A bump pushed to `refs/flywheel/deploy` **is advertised** by upload-pack and is
  **absent from `ls-remote --heads`** — so `git branch` and a default `git push`
  ignore it; "never pushed" is structural, not a convention.
- A fresh consumer **fetches it by full ref name** (`git fetch <url>
  refs/flywheel/deploy`) and resolves the bumped manifest. (Plain git does this
  fine; source-controller's go-git does **not** for a custom namespace — see the
  cluster smoke test below.)
- **Rebase-forward** (`git rebase --onto AUTHORED' <old base> DEPLOY`) is **clean**
  when the authored edit is away from the image line (verified: a `replicas` bump),
  and **conflicts** when it lands in the same hunk (verified: a container rename) —
  in which case the **reset-and-rebump fallback** yields the correct
  `AUTHORED'' + fresh bump`. Both paths exercised.

**Implementation caveat surfaced here:** the bare repos are seeded with
`git clone --bare` (`scripts/git-server/entrypoint.sh:74`), which copies only
`refs/heads/*` and `refs/tags/*` — so the DEPLOY ref must be **created by an explicit
push**, not assumed from the clone (folded into Component change 3).

**Cluster smoke test — the decisive result (rejects the non-head ref).** Installed
the pinned Flux into a throwaway k3d cluster (source-controller **v1.8.4** confirmed
from the running Deployment image) pointed at an in-cluster smart-HTTP git-server
serving a repo whose deploy commit sits on a `refs/flywheel/deploy` ref:
- `spec.ref.name: refs/flywheel/deploy` → **FAILS**: `GitOperationFailed … unable to
  resolve commit object for '<sha>': object not found`. Source-controller *resolves*
  the custom ref name to its SHA (from the advertisement) but go-git's fetch only
  transfers objects reachable from standard refs, so the custom-namespace tip never
  arrives. A non-head ref is **not usable** with the pinned Flux.
- `spec.ref.branch: flywheel/local-deploy` (the *same* commit, as a `refs/heads/`
  branch) → **Ready=True, Succeeded, `revision=flywheel/local-deploy@sha1:<sha>`**.

**Conclusion:** DEPLOY is a plain branch `flywheel/local-deploy`, tracked via
`spec.ref.branch` (universally supported — no version gate). The "never pushed"
property comes from DEPLOY living only in the in-cluster bare repo and the worktree
never tracking it, not from a non-standard namespace.

## Decisions

1. **DEPLOY ref representation → a plain branch `flywheel/local-deploy`. (revised
   2026-06-28 by the smoke test)** The non-head `refs/flywheel/deploy` form was tried
   and **rejected**: source-controller's go-git resolves a custom-namespace ref name
   but cannot fetch its object (`object not found`); the `refs/heads/` branch form
   goes Ready cleanly. Tracked via `spec.ref.branch` (no Flux-version dependency).
   "Never pushed" is preserved by *where* the branch lives — the in-cluster bare repo
   only, never fetched into the worktree, never on the developer's upstream — rather
   than by a non-standard namespace. (It's still fine for the IUA's bump history to be
   invisible to the developer: it's ephemeral machine state, not authored history.)
2. **DEPLOY scope → a singleton branch. (proposed)** One `flywheel/local-deploy` per
   local cluster, re-pointed to track whichever AUTHORED branch is current — *not*
   one deploy ref per authored branch. The ref means *"what this one local cluster
   is currently running,"* not durable per-branch history:
   - A developer has exactly one k3d cluster reconciling one desired state at a time.
     When they `flywheel use feat-y` (or switch the worktree), the cluster should now
     run `feat-y + bumps`; the previous `feat-x + bumps` state has no further
     consumer — its bumps were ephemeral local-registry tags, and feat-x's *authored*
     commits live safely on the `feat-x` branch regardless.
   - Per-branch deploy refs would accumulate dead refs full of stale tags that
     nothing reconciles, needing their own garbage collection. The singleton has none
     of that: on a switch it is simply rebuilt (`feat-y + bumps`) via the same
     rebase-forward / reset-and-rebump path as any AUTHORED advance.
3. **Reconciliation home → a Go controller that owns the *self* sync end-to-end.
   (accepted)** It replaces `sync.sh` *for `git-auto-sync-self` only*: worktree→bare
   push, DEPLOY maintenance (rebase-forward + suspend/resume + conflict fallback), and
   the reconcile pokes — one process, one poll loop, no cross-process race on DEPLOY,
   and the fiddly rebase logic becomes unit-testable. It lands alongside the existing
   Go controllers (`cmd/image-builder-controller`,
   `internal/controller/imagepolicy_iua_controller.go`).
   - **Do we also port `sync.sh`?** Only the *self* path, and by reimplementing its
     (now changed) behaviour in the controller — not a line-by-line port. The
     **per-app** `git-auto-sync-{app}` sidecars keep running `sync.sh` unchanged:
     Isolate doesn't touch app-code repos (the IUA never writes them), so porting them
     buys nothing here. A full port of the per-app fleet is a *separate* effort — the
     scaling lever noted for git-sync (issue #6) — and is **deferred**. End state of
     this design: self = Go controller, per-app = `sync.sh`, with a clear path to
     consolidate later.

## Edge cases & risks

- **sync ↔ IUA race on DEPLOY** — handled by suspend/resume around the rebuild.
- **Rebase conflict on the setter line** — fallback to reset-and-rebump; rare
  because the line is machine-managed.
- **Transient placeholder deploy** — only on the fallback path; near-instant
  re-bump via the ImagePolicy poke controller
  (`internal/controller/imagepolicy_iua_controller.go`).
- **`flywheel use` / branch switch** — the loop detects the configured-branch
  change and **resets** DEPLOY to the new branch (discarding the old branch's
  bumps); Flux ref is constant, so no repoint. Same `AUTO_FOLLOW_BRANCH=false`
  protection against transient checkouts.
- **Force-pushing the *same* AUTHORED branch mid-loop** (amend / interactive
  rebase): treated as an advance → rebase-forward. If the rewrite touches the same
  hunks as the bumps it conflicts → reset-and-rebump (correct); a non-conflicting
  rewrite can leave a few stale replayed commits on DEPLOY until the next clean
  advance (the deployed image is still correct). Known minor edge; precise
  handling would need DEPLOY-base tracking and is deferred.
- **Detached HEAD / fresh repo (no commits)** — no AUTHORED to track; DEPLOY not
  built; the loop is inert until a branch with a commit exists (matches today's
  `CurrentBranch` fallback intent).
- **Working on `main` is now safe** — bumps go to DEPLOY, never `main`; the reason
  the dev-branch guard existed disappears.
- **Teammate pulls the branch** — gets a portable manifest (placeholder / last
  real authored image); their own loop builds + bumps DEPLOY locally. No
  cross-machine tag references.

## Migration plan

1. **Revert the interim dev-branch guard** (built, currently uncommitted on the
   working tree):
   - delete `internal/cli/converge/devbranch.go` + `devbranch_test.go`;
   - revert `git.dev_branch` from `internal/cli/schema/schema.go` (+ `schema_test.go`),
     the skeleton `templates/client-skeleton/flywheel.yaml.tmpl`, and the init
     golden `internal/cli/initcmd/testdata/golden/default/flywheel.yaml`;
   - revert the `up` step-1b call (`internal/cli/up/up.go`) and the `use` redirect
     block (`internal/cli/usecmd/usecmd.go`).
2. Implement the DEPLOY model (component changes above).
3. No client-data migration: DEPLOY is created fresh in each local cluster on the
   next `up`; existing pushed branches are unaffected (and retroactively benefit —
   no new bumps land on them).

> **Follow-up (issue #27): prune the superseded in-cluster machinery.** This plan
> migrated the *source* model (DEPLOY ref) and removed the `git-auto-sync-self`
> *template*, but a removed template doesn't delete a Deployment already running
> from a prior `up` — so the old `git-auto-sync-self` was left Running and kept
> `git reset --hard`-ing the developer's worktree, re-polluting the very branch
> this design isolates. Resolved by `up` step 11e (`converge.PruneOrphanedMachinery`):
> every resource `up` applies imperatively is labelled `app.kubernetes.io/managed-by=flywheel`,
> and after applying, `up` deletes labelled machinery it did **not** re-apply this
> run. App/infra workloads (unlabelled, Flux-owned) and cascade/state kinds
> (Namespace, PVC, Secret, Flux Kustomization/GitRepository) are never touched.
> See `docs/dev/orphan-prune.md` for the contract. (Caveat: orphans from versions
> predating the label aren't auto-reaped — they were never labelled — so the live
> repro cluster's existing `git-auto-sync-self` needs a one-time manual delete.)

## Test plan

- **Unit (reconciler):** the `DEPLOY = AUTHORED + bumps` invariant across: fresh
  build; IUA bump with no authored change; authored advance (rebase-forward);
  rebase conflict (reset-and-rebump fallback); `use` switch. Hermetic temp git
  repos (the `repoOnBranch` helper pattern already used in the `converge` / `up` /
  `usecmd` tests).
- **Manifest:** rendered self-source tracks `flywheel/local-deploy` via
  `spec.ref.branch`; the IUA still has no `push.branch`.
- **Integration (cluster, if feasible in CI):** code change → image build → IUA
  bumps DEPLOY → rollout, with AUTHORED verified clean and a `git push` of
  AUTHORED carrying no bump commits.

### Live validation (done 2026-06-28)

A full `flywheel up` on a throwaway k3d cluster, then a local-only app added on a
`feat/app` branch and edited:
- `up` came up clean (5/5 Flux Kustomizations Ready); Flux's self GitRepository
  tracked `flywheel/local-deploy` (Ready).
- `git-deploy-controller` seeded DEPLOY and, on an authored commit, pushed it and
  rebuilt DEPLOY to follow — while `main` stayed bump-free.
- Adding + editing the `hello` app drove two real IUA bumps:
  `flywheel/local-deploy` accumulated `…0-placeholder → …<ts1> → …<ts2>` on top of
  the `add app hello` commit, the pod rolled to the latest tag, and **`feat/app`
  kept 0 bump commits** (its `deployment.yaml` still read `:0-placeholder`). The
  authored branch is never polluted; the bumps live only on DEPLOY.
- Found + fixed the two-apply-paths image gotcha (the Flux dev-loop reconcile path
  must rewrite the controller image too) — now covered by a render test.

## Open questions

1. ~~Confirm Flux `spec.ref.name` support; else fall back to a branch.~~ **Resolved
   2026-06-28 by a cluster smoke test** — the non-head `refs/flywheel/deploy` form
   **fails** (`object not found`); DEPLOY is the plain branch `flywheel/local-deploy`
   tracked via `spec.ref.branch`. See [Feasibility](#feasibility-resolved-2026-06-28),
   Decision 1.
2. ~~Go controller vs `sync.sh` for the reconciler.~~ **Resolved** — Go controller
   owning the self path; per-app `sync.sh` untouched (Decision 3).
3. ~~Should `flywheel use` warn when the configured AUTHORED branch ≠ the worktree's
   current checkout?~~ **Implemented** — `use` now warns ("your worktree is on X,
   not Y — commits you make now won't deploy until you `git switch Y`").
