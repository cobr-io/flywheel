# Joining an existing Flywheel repo

`flywheel init` is for creating a **new** environment. This guide is the other
path: a teammate already ran `init`, committed the repo, and pushed it — now you
want to bring the same environment up on your machine.

Everything an environment needs for **local** is committed — the ports, cluster
config, namespaces, Flux intervals, domain (all in `flywheel.yaml`), *and* the
local SOPS age key (at `clusters/local/age.key`). So onboarding is just clone
and up, with no key handoff:

```sh
# 1. Clone the existing gitops repo
git clone <repo-url> && cd <repo>

# 2. Bring up the cluster. --clone also materialises any app worktrees
#    declared in the workspace: block that aren't on disk yet.
flywheel up --clone
```

That's it — `flywheel up` reads the committed config from `flywheel.yaml`,
creates the k3d cluster and registry, installs Flux, decrypts the committed
secrets with the committed age key, and reconciles. `flywheel.yaml.local` is
optional: when it's absent, `workspaces_root` defaults to the parent directory
of the repo.

> **Do not run `flywheel init` in a clone.** `init` refuses a non-empty
> directory (it only tolerates a bare `.git`), and it's the wrong tool anyway —
> it re-allocates ports and re-scaffolds over the committed config. Cloning then
> running `up` is the whole onboarding flow.

> **Version skew on first up.** If your installed `flywheel` is older than the
> repo's pinned `flywheel.version`, `up` stops and asks you to upgrade the binary
> first; if it's newer, `up` offers to roll the pin forward. See
> [Upgrading flywheel & the version pin](upgrading.md).

## Why there's no key handoff (for local)

Flux decrypts the repo's SOPS-encrypted secrets (`clusters/local/*.enc.yaml`)
inside the cluster using a private age key. For the **local** cluster that key
is committed at `clusters/local/age.key` and is **canonical** — `flywheel up`
reads it straight from the repo (see `loadAgeKey` in `internal/cli/up/up.go`).
That's deliberate: the local key only ever decrypts `clusters/local/*` dev
secrets on your localhost cluster, so committing it lets anyone clone and `up`
with nothing to copy around.

Every **other** environment's key (prod, staging, …) stays out of git —
`.gitignore` keeps `clusters/*/age.key` except the local one, so those keys live
in your homedir, never the repo. Promoting to a real cluster is a separate,
manual flow.

> **Adding a secret scanner?** Flywheel doesn't ship one (no gitleaks, no
> push-protection config). If you add one — your own gitleaks, or your org runs
> GitHub secret scanning — allowlist `clusters/local/age.key`. It's an
> `AGE-SECRET-KEY` committed on purpose (it only ever decrypts `clusters/local/*`
> dev secrets), so a generic scanner will otherwise flag it as a leaked key.

> **Legacy repos.** If the repo was created before the local key was committed,
> it won't have `clusters/local/age.key`. In that case `up` falls back to the
> host key at `~/.config/flywheel/<client>/age.key` (where `<client>` is
> `client.name` from `flywheel.yaml`) and fails if it's missing — get that key
> from whoever set the repo up, place it there `chmod 600`, and run `up`. To opt
> a legacy repo into the no-handoff model, whoever holds the key can copy it to
> `clusters/local/age.key` and commit it (`.gitignore` already commits that one
> path).

## Caveat: port collisions

`flywheel up` uses the ports committed in `flywheel.yaml`
(`cluster.registry_port`, `http_port`, `https_port`) verbatim — it does **not**
consult `~/.config/flywheel/allocations.json` (only `init` writes that and only
`init` probes for free ports). If those committed ports collide with another
cluster already running on your machine, k3d fails with a raw socket-bind error
rather than re-allocating.

If that happens, free the conflicting ports (e.g. `flywheel down` the other
environment) or edit the ports in `flywheel.yaml`. Note that the ports are
committed and shared across the team, so changing them affects everyone —
prefer freeing the local conflict.
