# Adding a runtime image (image N+1)

Flywheel ships a fixed set of runtime container images (`schema.ImageNames`:
`git-server`, `git-auto-sync`, `image-builder-controller`,
`git-deploy-controller`). Adding a fifth touches several sites. Much of the
bootstrap wiring is now **derived** from `schema.ImageNames` and needs no edit;
the rest is this ordered checklist.

Two image *shapes* exist — the checklist calls out where they differ:

- **Controller** (Go binary; `image-builder-controller`, `git-deploy-controller`):
  a `cmd/<name>` package cross-compiled on the host and `COPY`'d into a
  distroless/debian image (issue #46 — no in-image QEMU Go build).
- **Script image** (`git-server`, `git-auto-sync`): shell/tooling only, built
  straight from the repo root, with its scripts declared as goreleaser
  `extra_files`.

The reference commit for a full walk-through is `5f1c7d7` (added
`git-deploy-controller`; note it spanned more than one commit historically —
this checklist is the complete set).

---

## Derived — do NOT edit (verify only)

These pick up a new `schema.ImageNames` entry automatically:

| Site | Why it's automatic |
|---|---|
| `templates/bootstrap/.../builders-kustomization.yaml.tmpl` and `client-builders-kustomization.yaml.tmpl` | Both `images:` blocks `range` over slices built from `schema.ImageNames` split by `bootstrapImageOwners`. |
| `internal/cli/converge/helpers.go` `renderDevLoopKustomization` (step 11a direct apply) | Loops over `schema.ImageNames`. |
| `internal/cli/imagepin/imagepin.go` (`Resolve`, mirror plan) | Loops over `schema.ImageNames`. |
| `.github/workflows/test.yml` `:latest` invariant check | Globs `git ls-files 'Dockerfile.*'`, so a new `Dockerfile.<name>` is scanned automatically (T04). |
| The image build in `.github/workflows/e2e-recipe.yml` and `scripts/e2e.sh` | Both call `make images` — the single build recipe. Adding to the Makefile `IMAGES` list (step 4) is enough. |

---

## The ordered checklist

### 1. `internal/cli/schema/schema.go`
Add the key to `var ImageNames`. This is the source of truth every derived site
keys off.

### 2. `internal/cli/converge/bootstrap.go` — `bootstrapImageOwners`
Assign the new image to exactly one Flux Kustomization:

- `imgOwnerDevLoop` — the image has a Deployment under the dev-loop overlay
  (`manifests/dev-loop/base`). This is the common case for a new controller.
- `imgOwnerClientBuilders` — the image's only Deployments are the per-app
  builder sidecars (this is why `git-auto-sync` is the odd one out).

Skipping this makes `TestBootstrapImages_TemplateUnionMatchesSchema` fail (the
image renders into neither template block) — that is the intended guard, not a
bug to route around.

### 3. `Dockerfile.<name>` (repo root)
- Controller: `COPY <name> /...` from the build context (the binary the Makefile
  and goreleaser host-build), on a `distroless`/`debian-slim` base.
- Script image: build from the repo root; declare scripts via goreleaser
  `extra_files` (step 7).

### 4. `cmd/<name>/` (controllers only)
Add the Go command package the binary builds from. Skip for a script image.

### 5. `Makefile`
Three spots in the `images` target region:
- Add `<name>` to `IMAGES`.
- Controller only: add `<name>` to the host cross-compile loop
  (`for c in image-builder-controller git-deploy-controller`) **and** to the
  `case` that selects the host-built context (`bctx="$ctx"`).

### 6. `manifests/dev-loop/base/` (dev-loop images only)
- Add `manifests/dev-loop/base/<name>.yaml` (the Deployment). Pin the image as
  `ghcr.io/cobr-io/<name>:rewritten-by-flywheel-up` — the placeholder tag `up`
  rewrites (T07).
- Add `./<name>.yaml` to `manifests/dev-loop/base/kustomization.yaml`
  `resources:`.
- (A `git-auto-sync`-shaped image instead lives in
  `manifests/per-app-template/`, not here.)

### 7. `.goreleaser.yaml`
Not derivable — edit by hand:
- Controller only: add a `builds:` block (host cross-compile;
  `CGO_ENABLED=0`, `GOOS=linux`, `-trimpath`, `-s -w`).
- Add **two** `dockers:` blocks (`-amd64` and `-arm64`). Controllers select the
  host-built binary via `ids: [<name>]`; script images use
  `extra_files: [scripts/<name>]`. Tags use `{{ .Tag }}` (leading `v`), never
  `{{ .Version }}`.
- Add **one** `docker_manifests:` entry joining the two arch tags.
- Bump the count in the header comment ("Four multi-arch container images …").

### 8. `.github/dependabot.yml`
The `docker` ecosystem globs `directory: "/"`, so the new base image is scanned
automatically — but update the comment that enumerates the four Dockerfiles so
it stays accurate.

### 9. `.github/workflows/e2e-recipe.yml`
The build step is `make images` (no change), but two hardcoded enumerations need
the new image:
- the `flywheel.yaml.local` dogfood override block (`<name>: flywheel-dev/<name>:ci`);
- the multi-node crictl re-import list.

### 10. `scripts/e2e.sh`
Add the image to the `flywheel.yaml.local` override block
(`<name>: flywheel-dev/<name>:$TAG`). The build is `make images` (no change).

---

## Verify

```sh
go vet ./... && go test ./...          # incl. the image agreement test
kubectl kustomize manifests/dev-loop/base/   # base still renders
make images IMAGE_TAG=ci               # all N images build
bash scripts/e2e.sh                    # or the CI e2e recipe
```
