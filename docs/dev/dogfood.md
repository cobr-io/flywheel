# Dogfood mode

"Dogfood mode" is for hacking on the four runtime images (`git-server`,
`git-auto-sync`, `image-builder-controller`, `git-deploy-controller`) themselves,
rather than running the published `ghcr.io/cobr-io/*` ones. You build them locally
and pin the refs via `flywheel.yaml.local` (gitignored, per-developer).

The canonical image list is `schema.ImageNames`; when a new image is added there,
pin it here too or `up` falls back to its (likely unpublished) ghcr.io default and
fails fast.

1. **Build the images** from the Flywheel source tree. `make install` (and
   `make images`) build all four as `flywheel-dev/<name>:dogfood`:

   ```sh
   cd ~/src/github.com/cobr.io/flywheel
   make images               # or `make install` for binary + images + completions
   ```

   The tag defaults to `dogfood` to match step 2; override with
   `make images IMAGE_TAG=<tag>`.

2. **Pin the overrides** in your client repo's `flywheel.yaml.local`:

   ```yaml
   flywheel:
     images:
       git-server: flywheel-dev/git-server:dogfood
       git-auto-sync: flywheel-dev/git-auto-sync:dogfood
       image-builder-controller: flywheel-dev/image-builder-controller:dogfood
       git-deploy-controller: flywheel-dev/git-deploy-controller:dogfood
   ```

3. **`flywheel up`** sees the refs in your host docker store, skips the ghcr.io
   pull, and pushes them into the cluster's **local registry** under a
   content-addressed `dogfood-<id>` tag (so every node pulls on demand). To roll
   a new build into a *running* cluster, rebuild and re-run `flywheel up` — it's
   idempotent: it re-mirrors the rebuilt image and rolls the affected
   Deployments to the new content-addressed ref (no manual `kubectl delete pod`):

   ```sh
   make images && flywheel up
   ```

   To just pre-populate the registry without a reconcile:
   `make push-local REGISTRY_PORT=<your cluster.registry_port>`.

If an override is unset and the corresponding ghcr.io ref hasn't been published,
`flywheel up` fails fast with the exact override stanza you need to add.

If an override *is* pinned to a `flywheel-dev/<name>:<tag>` ref but you haven't
built it yet (you forgot step 1), `up` detects that up front — the ref names no
registry, so it can't be pulled — and stops with a "run `make images`" message
listing every missing image, instead of failing later with a cryptic in-cluster
`ImagePullBackOff`.

On colima/lima, your client repo must live under a host path the VM bind-mounts
(typically `~/`, not `/tmp`) so the host worktree is visible inside the cluster.

## Gotchas

These two trip everyone up at least once:

* **Flux reverts your `kubectl` edits — but not `:dogfood` image *content*.**
  On a live cluster Flux continuously reconciles to the manifest tree, so manual
  `kubectl edit` changes (RBAC, env vars, replica counts, …) get reverted within
  a reconcile interval. It does **not** revert the *content* of a `:dogfood`
  image you rebuilt under the same tag. When you need manifest edits to stick
  while iterating, **suspend the dev loop** first:

  ```sh
  flux suspend kustomization flywheel-dev-loop
  # ... iterate with kubectl ...
  flux resume kustomization flywheel-dev-loop
  ```

* **`flywheel up` can serve stale embedded manifests from the cache.** The CLI
  unpacks its embedded manifest tree into `~/.cache/flywheel/<version>` and the
  cache is **not** busted on content change — for dev binaries `<version>` is
  `v0.0.0-dev`, so a rebuild with edited embedded manifests keeps serving the
  old ones. When iterating on the embedded manifest tree, remove the cache:

  ```sh
  rm -rf ~/.cache/flywheel/v0.0.0-dev   # or ~/.cache/flywheel/<version>
  ```
