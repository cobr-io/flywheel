# Upgrading flywheel & the version pin

Every Flywheel repo pins a release in `flywheel.yaml`:

```yaml
flywheel:
  version: v0.2.0
```

That pin is the single source of truth for the control-plane images and the
embedded manifests `flywheel up` deploys — bumping it rolls the whole dev loop
forward in one line. The pin is **committed and shared** across your team; the
`flywheel` **binary** is installed per-developer. Run `flywheel version` to see
which binary you have.

Because the two can drift apart, `flywheel up` compares them *before* it does any
work and keeps them in agreement:

| Your installed `flywheel` vs the repo's `flywheel.version` | What `up` does |
| --- | --- |
| **Same** | Proceeds silently. |
| **Newer** — you upgraded, the repo is behind | Warns, then asks `update flywheel.version to <version>? [Y/n]`. Accept to roll the pin forward and continue; decline to abort without changing anything. |
| **Older** — your binary is behind the repo | **Stops.** Upgrade your `flywheel` binary and re-run; `up` won't run an older binary against a newer pin (it would deploy stale manifests against newer image tags). |

The check only ever moves the pin **forward**: the older-binary case has no code
path that writes `flywheel.yaml`, so an out-of-date binary can never roll the
team's pin backward.

## When the repo is behind your binary

Accepting the `[Y/n]` prompt is the smoothest upgrade — it rewrites
`flywheel.version` for you (your inline comments are preserved). **Commit that
one-line change** so teammates pick up the same release on their next `up`. You
can also edit the pin by hand and re-run.

## When your binary is behind the repo

Install the newer `flywheel` release the same way you installed it originally,
then re-run `up`. There's no way to make an older binary deploy a newer pin —
that's the whole point of the stop.

> **Building from source?** A `flywheel` built with `make install` isn't stamped
> with a clean release tag, so `up` **skips this check entirely**. That's
> intended: it keeps the version gate out of your way while you're dogfooding or
> hacking on Flywheel itself.

## Migrating off the per-app `git-auto-sync` sidecar (existing repos)

Releases from the git-auto-sync Go port onward (issue #86) replace the
per-app `git-auto-sync-<app>` bash sidecar with a single shared
`git-auto-sync` controller Deployment in `flywheel-system` that watches every
app's `GitRepository` and drives the same branch-follow/mirror behavior,
race-free.

Bumping `flywheel.version` past that release is **not enough by itself** —
per-app files are never re-rendered by `flywheel up`, so an existing repo's
`builders/base/<app>/git-auto-sync.yaml` sticks around and its old sidecar
Deployment keeps running. To finish migrating each app:

```sh
git rm builders/base/<app>/git-auto-sync.yaml
git commit -m "migrate <app> off the git-auto-sync sidecar"
```

Flux (`client-builders`, `prune: true`) deletes the old per-app Deployment on
that commit. That's the whole migration — nothing else to change, and
**new** apps need nothing at all (`flywheel add app` on a current release no
longer renders the file in the first place).

**Why there's no rush, and no two-writer window:** the new shared controller
interlocks against the legacy file. While `builders/base/<app>/git-auto-sync.yaml`
is still present, it skips that app (logging a warning once) and lets the old
sidecar keep syncing it — with the old sidecar's known race, but never with
both writers touching the app's `GitRepository` at once. Migrate apps on
your own schedule; each one flips over the moment its file is deleted.

**Rollback:** reverting to an older `flywheel` release goes back to rendering
the sidecar on `add app`, but does not restore a `git-auto-sync.yaml` you
already deleted — re-run `add app` (re-renders it) or restore the file from
git history, then commit.
