#!/usr/bin/env bash
# `make images` must exit non-zero when an image fails to build.
#
# Issue #136: a transient gcr.io 500 failed the image-builder-controller
# `docker build`, but the recipe's for-loop swallowed it and `make images`
# printed "built: …" and exited 0. The e2e job then died a step later in
# `flywheel up` with a misleading "dogfood image(s) not found" error.
#
# This puts a failing `docker` stub first on PATH and asserts `make images`
# fails. The stub leaves a marker so a pass can't come from something else
# (e.g. a real `go build` break) failing before docker is reached. Run it
# locally exactly as CI does:
#   bash scripts/check-make-images-fails-fast.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

stub="$(mktemp -d)"
trap 'rm -rf "$stub"' EXIT
cat >"$stub/docker" <<EOF
#!/bin/sh
touch "$stub/docker-called"
echo "stub docker: simulated build failure" >&2
exit 1
EOF
chmod +x "$stub/docker"

if PATH="$stub:$PATH" make images IMAGE_TAG=fail-fast-check >/dev/null 2>&1; then
  echo "FAIL: make images exited 0 although docker build failed" >&2
  exit 1
fi
if [ ! -e "$stub/docker-called" ]; then
  echo "FAIL: make images failed before reaching docker build; check is inconclusive" >&2
  exit 1
fi
echo "ok: make images fails when docker build fails"
