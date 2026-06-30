# Dev-loop happy-path validation

A manual, copy-pasteable runbook that exercises the **entire inner loop** on a
throwaway cluster: `init → up → add app → commit → live reload`. Run it to
confirm the product still works end-to-end — before cutting a release, after
touching `up` / `add app` / the runtime images, or as a periodic smoke check.

It's written so a human *or* an agent can follow it step by step and decide
PASS/FAIL from explicit checks. Every command below was run as-is during the
v0.1.0 pre-release validation.

> There is also an automated harness: the CI `k3d-e2e` job
> (`.github/workflows/test.yml`) and the local `make e2e` (`scripts/e2e.sh`),
> which drive the `testdata/scenarios/` scripts (scenario 1 = baseline dev loop,
> 5 = orphan-job reaper; 2–4 cover branch switches). This runbook is the manual,
> human/agent-followable counterpart — handy for ad-hoc checks and for eyeballing
> `add app`'s scaffolding interactively.

## Prerequisites

- `git`, `k3d`, the `docker` CLI + a running daemon, and `mkcert`. Check with
  `flywheel doctor`.
- A `flywheel` binary built from the source under test, on `$PATH`:
  ```sh
  cd <flywheel-checkout> && make build      # stamps the version into flywheel.yaml
  ```
- Runtime images. Two options:
  - **Dogfood (validates current source — recommended):** build the four images
    locally and pin them (steps 0 + 2 below).
  - **Released:** once `vX.Y.Z` is tagged and the ghcr images are published, skip
    the dogfood pins — `up` pulls `ghcr.io/cobr-io/*:vX.Y.Z` automatically.

> **colima/lima:** the gitops repo and app worktree **must** live under a host
> path the VM bind-mounts (use `~/`, not `/tmp`). All paths below are under `~/`.

## Variables

```sh
export NAME=relcheck                       # client/cluster name (keep it unique)
export ROOT="$HOME/.flywheel-$NAME"        # workspaces_root: parent of gitops + app
export GITOPS="$ROOT/$NAME"                # the gitops repo
export APP=sample-app
export APPDIR="$ROOT/$APP"                 # sibling app worktree
export KCTX="k3d-$NAME-local"
```

## Step 0 — build the dogfood images (skip if validating a published release)

```sh
cd <flywheel-checkout>
make images IMAGE_TAG=dogfood     # builds all 4: git-server, git-auto-sync,
                                  # image-builder-controller, git-deploy-controller
docker images | grep flywheel-dev   # confirm 4 :dogfood tags exist
```

## Step 1 — scaffold the gitops repo

```sh
mkdir -p "$GITOPS" && cd "$GITOPS"
flywheel init --org=cobr-io
```
Expect: `initialised …`, a `ports: registry=… http=… https=…` line, and an age
key path. `flywheel.yaml` now exists with `cluster.name: $NAME-local`.

## Step 2 — pin the dogfood images (skip for a published release)

```sh
cat > "$GITOPS/flywheel.yaml.local" <<'EOF'
flywheel:
  images:
    git-server: flywheel-dev/git-server:dogfood
    git-auto-sync: flywheel-dev/git-auto-sync:dogfood
    image-builder-controller: flywheel-dev/image-builder-controller:dogfood
    git-deploy-controller: flywheel-dev/git-deploy-controller:dogfood
EOF
```
All **four** images must be pinned — `schema.ImageNames` is the canonical list.
A missing pin falls back to the (possibly unpublished) ghcr ref and `up` fails.
`flywheel.yaml.local` is gitignored (per-developer).

## Step 3 — bring the cluster up, then assert everything is Ready

```sh
cd "$GITOPS" && flywheel up
```
Expect it to finish with `Cluster up. Add an app: …` and a `5/5 … Ready` line.
Then assert:

```sh
# All Flux Kustomizations Ready=True (expect 5: client-apps, client-builders,
# client-infra, flywheel-dev-loop, flywheel-infra)
kubectl --context=$KCTX get kustomization -A

# Both GitRepositories Ready=True (flux-system + flywheel)
kubectl --context=$KCTX get gitrepository -A

# No pod stuck out of Ready (empty output = all good)
kubectl --context=$KCTX get pods -A --no-headers \
  | awk '$4!="Running" && $4!="Completed"{print} $4=="Running"{split($3,a,"/"); if(a[1]!=a[2]) print "NOT READY:",$0}'
```
**PASS:** 5/5 Kustomizations `READY=True`, 2/2 GitRepositories `READY=True`, the
last `awk` prints nothing. (`helm-install-traefik-*` Jobs showing `Completed` is
normal k3d.)

## Step 4 — create a sibling app with a simple Dockerfile

```sh
rm -rf "$APPDIR" && mkdir -p "$APPDIR"
cat > "$APPDIR/Dockerfile" <<'EOF'
FROM busybox:1.37
COPY index.html /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
EOF
echo "init" > "$APPDIR/index.html"

git -C "$APPDIR" -c init.defaultBranch=main init -q
git -C "$APPDIR" -c user.email=val@flywheel.test -c user.name=val add -A
git -C "$APPDIR" -c user.email=val@flywheel.test -c user.name=val commit -q -m "init $APP"
# add-app treats an app with no remote as local-only and refuses it on the
# integration branch. A placeholder origin is enough (the in-cluster loop syncs
# via git-server, not origin):
git -C "$APPDIR" remote add origin "https://git.example.test/$NAME/$APP.git"
```

## Step 5 — add the app to the gitops repo, commit, wait for it to come up

```sh
cd "$GITOPS"
flywheel add app "$APP"           # scaffolds builders/base/$APP + apps/base/$APP
git -c user.email=val@flywheel.test -c user.name=val add -A
git -c user.email=val@flywheel.test -c user.name=val commit -q -m "add $APP"
```
Flux pulls + applies within ~10s; the cold path (git-auto-sync mirror → image
build → IUA bump → rollout) typically lands the first pod within ~1–2 min. Watch:

```sh
kubectl --context=$KCTX -n apps get deploy,pods -l app=$APP -w   # Ctrl-C when 1/1
# Confirm it serves the committed content:
pod=$(kubectl --context=$KCTX -n apps get pods -l app=$APP --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
kubectl --context=$KCTX -n apps exec "$pod" -- cat /www/index.html   # → init
```
**PASS:** `deploy/$APP` is `1/1` and serves `init`.

## Step 6 — edit the source, commit, measure commit → live

```sh
NEW="hello-v2-$(date +%s)"
OLDIMG=$(kubectl --context=$KCTX -n apps get deploy $APP -o jsonpath='{.spec.template.spec.containers[0].image}')
echo "$NEW" > "$APPDIR/index.html"
T0=$(date +%s)
git -C "$APPDIR" -c user.email=val@flywheel.test -c user.name=val commit -aqm "set $NEW"

# Poll every running pod (the pod name changes on rollout) until the new text serves:
while (( $(date +%s) - T0 < 180 )); do
  for p in $(kubectl --context=$KCTX -n apps get pods -l app=$APP --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}'); do
    [ "$(kubectl --context=$KCTX -n apps exec "$p" -- cat /www/index.html 2>/dev/null)" = "$NEW" ] \
      && { echo "commit → served: $(( $(date +%s) - T0 ))s"; break 2; }
  done
  sleep 2
done
```
**PASS:** the new text serves within the loop. Reference timings on colima:
**~20s** for the first warm cycle, **~5s** fully warm. (Only *committed* changes
trigger a build — git-auto-sync ignores a dirty worktree.)

## Step 7 — tear down

```sh
cd "$GITOPS" && flywheel down --yes      # deletes the cluster + registry, releases the port allocation
rm -rf "$ROOT"
```

## Known gotchas

- **Pre-release version pin.** With no tags published, `init` pins
  `flywheel.version` to the git build id (e.g. `1565109`), not a `vX.Y.Z` tag.
  Expected; after tagging, confirm `init` pins the real tag.
- **`git-auto-sync` is commit-driven.** A saved-but-uncommitted file change does
  not trigger a build; commit to drive the loop.

## Notes for agents

- All commands are deterministic. Drive PASS/FAIL off the explicit checks in
  steps 3, 5, 6 (Kustomization/GitRepository `READY`, served `index.html`).
- Use a **unique** value (e.g. a timestamp) for the step-6 edit so the served
  content can't accidentally match a previous run.
- Re-query the running pod each poll — its name changes on rollout.
- Keep `$NAME` unique so the cluster/ports don't collide with an existing one.
- Always run step 7 (teardown) on exit, including on failure, to avoid leaking a
  cluster + ports.
