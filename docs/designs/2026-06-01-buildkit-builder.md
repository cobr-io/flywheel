# Replace Kaniko with a warm BuildKit daemon for the local build loop

**Status:** implemented
**Date:** 2026-06-01
**Author:** matthijs (with collaboration from Claude)

> **Outcome (implemented & measured on paritytest, heavy multi-stage Go image):**
> warm build (code-only change) **38s Kaniko → 12s BuildKit** (buildctl pure
> ~7s = just the `go build`; dep layer is a daemon-cache hit), cold **45s →
> 32s**; end-to-end commit→pod warm **~9-18s** (spread is the deploy back-half,
> not the build). One real bug surfaced only in live testing and is fixed in
> the shipped template: rootless UID mismatch on the shared workspace
> (`permission denied` on the Dockerfile) → pod `runAsUser/fsGroup: 1000`.
> Decisions made during implementation: **no buildkitd.toml** needed
> (`registry.insecure=true` on the buildctl output suffices — buildkitd is
> fully client-agnostic); the build container keeps the name `kaniko` so the
> image-scan controller is untouched; `moby/buildkit` pulls on demand (offline
> pre-import deferred — see Open questions). The Kaniko engine was removed
> outright rather than kept behind a switch (the fallback in § Migration was
> not implemented — revisit if a client hits a BuildKit edge case).
>
> **Build-context mechanism changed from the spike/approach below.** The
> sections that follow describe fetching the **Flux source artifact tarball**
> into a local context via a `fetch-source` initContainer (`--local
> context=/ctx`). The shipped build instead uses BuildKit's **remote git
> context** — `buildctl --opt context=<repoURL>#<fullSHA>[:<subdir>]`, so
> buildkitd clones the ref server-side and the build Pod is a pure thin
> `buildctl` client with no initContainer or shared workspace volume (see
> `gitContextURL` in `internal/controller/gitrepository_build_controller.go`).
> Provenance is unchanged: the ref is the exact full commit SHA Flux observed,
> so what's built is the same revision the artifact path would have delivered.
> This trades the spike's "no git frontend" simplification (Spike result #3)
> for a simpler build Pod; it does mean the build re-fetches from the source
> git URL rather than the Flux artifact tarball.

## Problem

The local dev loop builds container images with Kaniko: the
`image-builder-controller` creates one Kaniko `Job` per observed commit
(`internal/controller/templates/kaniko-job.yaml`), which fetches the Flux
source artifact, builds, and pushes to the in-cluster k3d registry.

After the parity-loop latency work (event-triggered reconciles +
`--requeue-dependency` + container-exit scan poke), the **build itself is the
dominant term** in commit-to-pod. On a trivial image that's ~3s and fine. On a
realistic image it is not — and real client images are large multi-stage
Dockerfiles (heavy base, dependency install, compile), not toy ones.

Measured on the dogfood `paritytest-app` (multi-stage Go: `golang:1.23` builder
+ `go mod download` + compile → distroless), on the live k3d cluster:

| Scenario | Kaniko |
|----------|--------|
| cold (no cache) | **45s** |
| warm (code-only change, dep layer is a cache *hit*) | **38s** |

The warm number is the damning one: a one-line code change costs 38s **even
though Kaniko reports a cache hit on the `go mod download` layer**. The kaniko
logs show why — on a hit it still:

* spends **~13s extracting the cached layer** from the registry to the build
  filesystem, then
* recompiles (~11s, uncacheable — code changed), then
* **pushes layer tarballs (~5s)**.

Kaniko's whole-filesystem-snapshot model means "cache hit" still pays a heavy
materialize + push tax every build. That tax is structural, not tunable.

## Goal

Cut the warm build (the inner-loop case: edit code, rebuild) from ~38s to
roughly the compile time alone, without giving up the GitOps-parity properties
the dev loop relies on (build triggered by an observed Flux commit, image
pushed to the same registry, same `<ts>-<shortSHA>` tag the ImagePolicy
selects).

## Spike result (already run — this de-risks the design)

A warm rootless `buildkitd` Deployment was applied to the paritytest cluster by
hand, and a build was driven through `buildctl` using the **Flux source
artifact tarball as the build context** (fetch + extract + `--local context`),
pushing to the same `k3d-paritytest-local-registry:5000` destination.

| Scenario | Kaniko | BuildKit (spike) |
|----------|--------|------------------|
| cold | 45s | **32s** |
| warm (code-only) | 38s | **8s** |

The warm 8s decomposed as: `go mod download` layer **`CACHED`** (instant, no
registry extract), `go build` 7.4s, export+push 0.2s. That is ~4.75× on the
case that matters, and it directly removes the ~13s extract + ~5s push tax.

Three unknowns the spike retired:

1. **Rootless works on k3d** — `moby/buildkit:*-rootless` with
   `--oci-worker-no-process-sandbox`, `seccompProfile: Unconfined`,
   `runAsUser: 1000`; **no privileged container** required.
2. **Insecure HTTP registry works** — `registry.insecure=true` on the output
   plus a `buildkitd.toml` `[registry."k3d-...:5000"] http = true`.
3. **The Flux artifact tarball is a usable context** — no need for BuildKit's
   git frontend (which wanted a git ref; our source is a tarball URL). Fetch +
   untar + `--local` is enough, so the existing artifact-delivery path is
   reused unchanged.

## Approach

Replace the per-build Kaniko Job (which *is* the builder) with a **long-lived
warm `buildkitd`** plus a thin per-build **`buildctl` client Job**.

```
                        ┌─────────────────────────────┐
  GitRepository commit  │ image-builder-controller     │
  observed ───────────► │  renders a buildctl client   │
                        │  Job per builds[] entry      │
                        └──────────────┬──────────────┘
                                       │ creates
                                       ▼
        ┌──────────────────────────────────────────────┐
        │ build-<repo>-<img>-<ts>-<sha> Job (client)    │
        │  initContainer: fetch+untar artifact → /ctx   │
        │  container: buildctl --addr tcp://buildkitd \ │
        │    build ... --local context=/ctx push=true   │
        └───────────────────────┬──────────────────────┘
                                 │ gRPC :1234
                                 ▼
        ┌──────────────────────────────────────────────┐
        │ buildkitd Deployment (warm, 1 replica)        │
        │  - rootless, --oci-worker-no-process-sandbox  │
        │  - buildkitd.toml: k3d registry http=true     │
        │  - PVC: /home/user/.local/share/buildkit      │ ← warm cache lives here
        └──────────────────────────────────────────────┘
```

Why warm-daemon-plus-client rather than a self-contained per-build buildkit
Job: the cache (and thus the entire win) lives in the daemon's snapshot store.
A per-build daemon cold-starts an empty cache every time and reproduces the
Kaniko problem. The standing daemon keeps the dep layer hot across commits.

The client Job stays a Job (not a bare Pod) so the existing reap-on-GR-delete
path (`reapJobsForRepo`) and the `app/repo/image/commit` labels are unchanged —
which means **the B1 scan-poke (watch build Pods, poke on `kaniko` container
exit) keeps working** with one rename: the build container is no longer named
`kaniko`. See § Controller changes.

### Components

**1. `buildkitd` manifests** (`manifests/dev-loop/base/`)
- `Deployment` (1 replica, rootless image, the securityContext above).
- `Service` on :1234 (`buildkitd.flywheel-system.svc`).
- `PersistentVolumeClaim` `buildkit-cache` (RWO, 10Gi) for the snapshot store
  — replaces the role of the current `kaniko-cache` PVC.
- `ConfigMap` `buildkitd-config` holding `buildkitd.toml`. The registry host
  is client-specific (`k3d-<cluster.registry>:5000`), so the toml is
  **templated from `flywheel-config`** the same way other dev-loop values are,
  OR rendered by `flywheel up` — see § Open questions (the dev-loop overlay is
  kustomize-pure today; the registry name flows via the `flywheel-config`
  ConfigMap, so the cleanest route is an envsubst-free
  `[registry."k3d-${reg}:5000"]` materialised at up-time, mirroring how image
  refs are already rewritten).

**2. Build-job template** — replace `templates/kaniko-job.yaml` with a buildctl
client template:
- initContainer `fetch-source` (unchanged: alpine wget+untar of `ArtifactURL`
  → `/workspace`).
- container `build` (renamed from `kaniko`): `moby/buildkit:<ver>` image, runs
  `buildctl --addr tcp://buildkitd.flywheel-system:1234 build
  --frontend dockerfile.v0 --local context=/workspace/<context>
  --local dockerfile=/workspace/<context>
  --opt filename=<dockerfile>
  --output type=image,name=<Destination>,push=true,registry.insecure=true
  --export-cache type=inline`.
- No `kaniko-cache` volume on the client (cache is daemon-side now). The
  CPU-limit knob (`BuildCPULimit`) moves to the buildkitd daemon, not the
  client (the daemon does the work).

**3. Controller changes** (`gitrepository_build_controller.go`,
`buildjob_imagescan_controller.go`)
- `renderJob` is unchanged in shape (same `renderCtx`, same labels, same
  `Destination`); only the embedded template differs.
- The B1 scan poke keys off the container named `kaniko`
  (`kanikoContainerName`). Rename the constant/usage to the new build
  container name (e.g. `build`), or keep the container named `kaniko` to avoid
  touching the controller. **Recommend keeping the container name `kaniko`** so
  `buildjob_imagescan_controller.go` needs zero changes — the poke fires on
  that container's exit-0 exactly as today.

**4. `flywheel up`** — the registry-mirror step that imports
`image-builder-controller` etc. now also needs the `moby/buildkit` image
available in-cluster (it's pulled from docker.io; either let containerd pull it
on first use, or mirror it into the local registry like the other dev-loop
images for offline parity — consistent with the existing image-resolution design).

### Parity properties (must hold)

* Build is still triggered only by a Flux-observed commit (controller watches
  `GitRepository.status.artifact`). Unchanged.
* Image still pushed to the same registry with the same
  `<ts>-<shortSHA>` tag → ImagePolicy/IUA path unchanged.
* The build still consumes the **Flux source artifact**, not the host worktree
  directly — so what's built is what Flux observed, preserving the
  "build from the committed revision" property.
* No privileged container introduced (rootless).

## Test plan

* **Unit:** template renders valid Job YAML with the buildctl args for
  representative `builds[]` entries (context/dockerfile/destination); the
  existing `renderJob` golden-style assertions extended.
* **Controller:** unchanged B1 tests still pass (container-exit poke), assuming
  the `kaniko` container name is retained.
* **Live (dogfood paritytest), against the recorded Kaniko baseline:**
  - cold build ≤ ~32s (spike), warm code-only ≤ ~10s.
  - full **commit→pod** on the heavy image: target ≤ ~15s warm (vs Kaniko
    ~45s+), measured with the existing `decomp.sh`/`fire.sh` harness.
  - multi-app: two apps building near-simultaneously share one daemon (RWO PVC
    is on the daemon now, not per-build — the current "serialise on RWO"
    caveat changes; confirm no corruption / acceptable serialisation).
* **Cold-start of the daemon:** first build after `flywheel up` waits for
  buildkitd readiness; confirm the controller/Job tolerates buildkitd not-yet-
  ready (retry, not hard fail).

## Migration / rollout

* Additive: add buildkitd manifests to `manifests/dev-loop/base`; swap the
  build-job template. No client-repo changes (the per-app builder folders and
  `build-config.yaml` schema are unchanged — same `image/context/dockerfile`).
* Ships as a normal Flywheel version bump; clients get it on `flywheel update`
  + `up`.
* **Fallback:** keep the Kaniko template embedded for one release behind a
  `flywheel.yaml` switch (e.g. `build.engine: kaniko|buildkit`, default
  buildkit) so a client hitting a BuildKit edge case can revert without a
  Flywheel downgrade. Remove the Kaniko path a release later if unused.

## Open questions

1. **buildkitd.toml templating.** The registry host is client-specific. The
   dev-loop overlay is kustomize-pure (params via `flywheel-config`); cleanest
   is to materialise the toml at `flywheel up` time like image refs, vs. a
   ConfigMap generator. Decide during implementation.
2. **Cache eviction.** The daemon cache PVC grows; BuildKit has
   `--oci-worker-gc` / `gckeepstorage`. Set a sane cap (e.g. keep 8Gi of 10Gi)
   so the PVC doesn't fill. (The existing registry-GC open issue is adjacent.)
3. **Daemon as SPOF / resource floor.** A standing buildkitd holds memory even
   when idle (request 512Mi, limit 4Gi). Acceptable for a dev cluster; note it.
4. **Multi-arch.** Spike built arm64 on an arm64 node; clients on amd64 hosts
   build amd64. buildkitd advertises both via emulation but we only ever want
   native — pin `--opt platform=linux/<native>` or rely on default (native).
   Confirm no accidental emulated (slow) builds.
5. **Concurrency.** One daemon, RWO cache PVC: simultaneous multi-app builds
   share the daemon's parallelism but the PVC is single-writer (the daemon).
   Should be fine (one writer = the daemon) but validate under the dogfood
   multi-app scenario.
