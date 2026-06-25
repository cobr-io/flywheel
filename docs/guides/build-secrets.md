# Build secrets

Some builds need a credential at **build time** — cloning a private Go module,
hitting a private pip/npm index, fetching a license. Baking that into a layer
(`ENV TOKEN=…`, a `COPY`ed file) leaks it into the pushed image and its history.
Flywheel exposes a credential the history-safe way: as a **BuildKit
`--secret`**, mounted only for the lifetime of one `RUN` and never written to a
layer.

## How it works

1. You store the credential in a Kubernetes Secret in **`flywheel-system`** (the
   builder namespace), next to the app's `GitRepository` and build-config.
2. You reference it from the app's build-config under `secrets:`.
3. On each build, `image-builder-controller` validates the reference
   (fail-closed — see below), then renders a build Job that **projects** the
   Secret key as a file into the thin `buildctl` client Pod and passes
   `--secret id=<id>,src=/run/build-secrets/<id>`.
4. `buildctl` reads that file **client-side** and streams it to the warm
   `buildkitd` daemon over the build session. The daemon never touches the
   client filesystem, and the secret is available to the Dockerfile only via an
   explicit `--mount`.

## 1. Create the Secret

```sh
kubectl -n flywheel-system create secret generic ci-creds \
  --from-literal=token="$GITHUB_TOKEN"
```

The Secret must live in `flywheel-system`. The controller has a namespaced
`get` Role there (it reads the key during validation); the Pod mount itself is
done by the kubelet.

## 2. Reference it in build-config

In `builders/base/<app>/build-config.yaml` (or the scaffolded
`<app>-build-config` ConfigMap), add a `secrets:` list to the build:

```yaml
builds:
  - image: my-app
    context: "."
    dockerfile: Dockerfile
    secrets:
      - id: GITHUB_TOKEN      # what the Dockerfile reads
        src: ci-creds/token   # <secretName>/<key> in flywheel-system
```

- **`id`** is the BuildKit secret id. It doubles as the mounted file name, so it
  must be a single safe path component — no `/`, and not `.`, `..`, or a
  leading-dot name. Use the convention your Dockerfile expects (e.g.
  `GITHUB_TOKEN`).
- **`src`** is exactly `<secretName>/<key>` (one `/`). Both halves must be
  non-empty; the name is a Secret in `flywheel-system` and the key is a data key
  within it.

A top-level `secrets:` (a sibling of `builds:`) applies to every build that does
**not** declare its own. Inheritance is **replace, not merge**: a build with its
own `secrets:` does *not* also get the top-level ones — list everything it needs.

## 3. Consume it in the Dockerfile

```dockerfile
# syntax=docker/dockerfile:1
RUN --mount=type=secret,id=GITHUB_TOKEN \
    GITHUB_TOKEN="$(cat /run/secrets/GITHUB_TOKEN)" \
    go mod download
```

The secret is present only inside that `RUN`; it is not in the final image, any
layer, or the build history.

## Validation (fail-closed)

Before creating a build Job the controller verifies, for each referenced secret,
that the Secret exists in `flywheel-system`, the key is present, and **its value
is non-empty** (an empty token otherwise fails opaquely mid-build). If any check
fails the build is **not** started and the GitRepository is requeued — so a build
that races ahead of its Secret recovers automatically once the Secret lands. The
secret value is read only to check non-emptiness; it is never logged.

## Caveats

- **The secret solves auth, not egress.** `buildkitd` still needs network
  reachability to wherever you're fetching from (e.g. `github.com`, your private
  index). This feature does not change the daemon's egress.
- **Mode.** The projected file is mounted `0444` (world-readable *inside the
  ephemeral, single-tenant build Pod*) so the rootless `buildctl` user (UID
  1000) can read the root-written file. This is a deliberate trade for a
  throwaway Pod, not a default.
