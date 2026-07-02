#!/usr/bin/env bash
# Doctor coverage (issue #38): exercises `flywheel doctor` end-to-end,
# including the failure path that no other scenario touches.
#
# Unlike scenarios 1-5 this needs NO cluster — `doctor` only probes host
# prerequisites — so it runs cheaply BEFORE `up` in CI (and standalone on
# any dev box that has git/k3d/docker/mkcert installed). It is therefore
# deliberately NOT part of run-all.sh (the numbered in-cluster dev-loop
# sequence); CI invokes it directly. It asserts:
#   1. `doctor --quick` exits 0 when the four prereqs are present (the
#      minimal set `up` step 2 gates on).
#   2. full `doctor` runs the extra full-only checks (proves quick != full).
#   3. ERROR PATH: with a required binary hidden from PATH, `doctor --quick`
#      exits non-zero AND names the missing binary in its output.
#
# Requires: a `flywheel` binary on PATH, plus git, k3d, docker (+ a running
# daemon) and mkcert — the same prereqs `up` needs. No env vars, no cluster.
set -euo pipefail

FW="$(command -v flywheel)"
log() { echo "[$(date '+%H:%M:%S')] $*"; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

log "scenario doctor: host-prerequisite checks"

# 1. Happy path — all four quick-mode prereqs are present here.
if ! "$FW" doctor --quick; then
  log "FAIL: 'doctor --quick' exited non-zero but git/k3d/docker/mkcert should all be present"
  exit 1
fi
log "doctor --quick OK (git, k3d, docker, mkcert all present)"

# 2. Full mode runs strictly more checks than --quick. Don't gate on its exit
#    code (pre-commit / yq are optional dev conveniences that may be absent in
#    CI); just prove the full-only port-collision check is wired in, so a
#    regression that silently drops full-mode checks is caught.
full_out="$work/full.txt"
"$FW" doctor >"$full_out" 2>&1 || true
if ! grep -q 'no foreign process holds an allocated port' "$full_out"; then
  log "FAIL: full 'doctor' did not run the full-only port-collision check"
  cat "$full_out"
  exit 1
fi
log "doctor (full) ran the full-only port-collision check"

# 3. Error path — hide mkcert from PATH and confirm doctor fails loudly.
#    A symlink farm exposes every prereq EXCEPT mkcert (non-mutating: the real
#    binaries are never touched), so mkcert is the one guaranteed failure and
#    the assertion stays crisp even if another probe also trips.
farm="$work/bin"
mkdir -p "$farm"
for b in git k3d docker; do
  ln -s "$(command -v "$b")" "$farm/$b"
done
err_out="$work/err.txt"
if PATH="$farm" "$FW" doctor --quick >"$err_out" 2>&1; then
  log "FAIL: 'doctor --quick' exited 0 with mkcert hidden from PATH"
  cat "$err_out"
  exit 1
fi
if ! grep -qi 'mkcert' "$err_out"; then
  log "FAIL: doctor failure output did not name the missing 'mkcert' binary"
  cat "$err_out"
  exit 1
fi
log "doctor --quick error path OK (non-zero exit + names missing mkcert)"

log "scenario doctor PASS"
