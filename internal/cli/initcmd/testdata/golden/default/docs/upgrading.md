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
