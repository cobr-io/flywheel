#!/usr/bin/env bash
# Run the k3d-e2e suite locally — the same flow as the `k3d-e2e` job in
# .github/workflows/test.yml: build the four runtime images, `flywheel init` +
# `up` into a throwaway cluster, run scenarios 1 + 5, then `down`.
#
# Requires a `flywheel` binary on PATH (run `make build`, or use `make e2e`),
# plus k3d, docker, and mkcert. The client repo lives under a host path the
# Docker VM bind-mounts (default ~/.flywheel-e2e; override with E2E_ROOT). Uses a
# distinct client name (default `acme`) so it never collides with your dogfood
# cluster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
E2E_ROOT="${E2E_ROOT:-$HOME/.flywheel-e2e}"
CLIENT_NAME="${CLIENT_NAME:-acme}"
TAG="${E2E_IMAGE_TAG:-ci}"
CLIENT_REPO="$E2E_ROOT/$CLIENT_NAME"

command -v flywheel >/dev/null || { echo "flywheel not on PATH — run 'make build' first." >&2; exit 1; }

echo "==> [1/4] building runtime images (flywheel-dev/*:$TAG)"
# The two controller images COPY a host-built binary rather than compiling Go
# in-image (issue #46). Cross-compile them for GOOS=linux/$(host arch) — the
# arch docker builds by default here — into a throwaway context dir; the
# script-only images still build from the repo root.
CTRL_CTX="$(mktemp -d)"
for c in image-builder-controller git-deploy-controller; do
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o "$CTRL_CTX/$c" "./cmd/$c")
done
for img in git-server git-auto-sync image-builder-controller git-deploy-controller; do
	case "$img" in
		image-builder-controller|git-deploy-controller) bctx="$CTRL_CTX" ;;
		*) bctx="$REPO_ROOT" ;;
	esac
	docker build -q -t "flywheel-dev/$img:$TAG" -f "$REPO_ROOT/Dockerfile.$img" "$bctx" >/dev/null
done
rm -rf "$CTRL_CTX"

echo "==> [2/4] fresh client repo at $CLIENT_REPO (cluster ${CLIENT_NAME}-local)"
k3d cluster delete "${CLIENT_NAME}-local" >/dev/null 2>&1 || true # clean any leftover
rm -rf "$CLIENT_REPO"
mkdir -p "$CLIENT_REPO"
cleanup() {
	echo "==> teardown: flywheel down"
	(cd "$CLIENT_REPO" 2>/dev/null && flywheel down --yes) >/dev/null 2>&1 || true
}
trap cleanup EXIT

(
	cd "$CLIENT_REPO"
	flywheel init --org=cobr-io
	cat >>flywheel.yaml.local <<EOF
flywheel:
  images:
    git-server: flywheel-dev/git-server:$TAG
    git-auto-sync: flywheel-dev/git-auto-sync:$TAG
    image-builder-controller: flywheel-dev/image-builder-controller:$TAG
    git-deploy-controller: flywheel-dev/git-deploy-controller:$TAG
EOF
	# Issue #1: squat the just-allocated http_port so `up` step 5b must heal
	# the collision; the scenarios then prove the cluster came up on the new
	# port. (Matches the k3d-e2e CI job.)
	INIT_HTTP_PORT=$(awk '/^[[:space:]]*http_port:/{gsub(/[^0-9]/,"",$2); print $2; exit}' flywheel.yaml)
	echo "==> squatting http_port $INIT_HTTP_PORT (0.0.0.0) before up"
	python3 -m http.server "$INIT_HTTP_PORT" --bind 0.0.0.0 >/dev/null 2>&1 &
	SQUAT_PID=$!
	for _ in $(seq 1 20); do
		if (exec 3<>/dev/tcp/127.0.0.1/"$INIT_HTTP_PORT") 2>/dev/null; then exec 3>&- 3<&-; break; fi
		sleep 0.25
	done
	echo "==> [3/4] flywheel up"
	flywheel up
	kill "$SQUAT_PID" 2>/dev/null || true
	HEALED_HTTP_PORT=$(awk '/^[[:space:]]*http_port:/{gsub(/[^0-9]/,"",$2); print $2; exit}' flywheel.yaml)
	[ "$HEALED_HTTP_PORT" != "$INIT_HTTP_PORT" ] ||
		{ echo "FAIL: http_port $INIT_HTTP_PORT squatted but up did not heal it" >&2; exit 1; }
	echo "==> host-port heal OK: http_port $INIT_HTTP_PORT → $HEALED_HTTP_PORT"
)

echo "==> [4/4] scenarios 1 + 5"
export KCTX="k3d-${CLIENT_NAME}-local"
export CLIENT_REPO
export WORKSPACES_ROOT="$E2E_ROOT"
export CLIENT_NAME
bash "$REPO_ROOT/testdata/scenarios/scenario-1-baseline.sh"
bash "$REPO_ROOT/testdata/scenarios/scenario-5-orphan-job-reaper.sh"

echo "==> k3d-e2e PASSED"
