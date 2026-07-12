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
#   4. WARN PATH (T25 — doctor severity levels): with only advisory tooling
#      (pre-commit/yq/certutil, documented as never gating `up`) missing,
#      full `doctor` still exits 0 — SeverityWarn findings are printed but
#      don't fail the run; only SeverityError does.
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

# 4. Warn path — expose only the four quick-mode prereqs (git/k3d/docker/
#    mkcert) so pre-commit, yq and (on Linux) certutil are guaranteed
#    absent from PATH. Run with a throwaway HOME so the allocations file
#    is empty (no port collisions) from a throwaway cwd that isn't a
#    flywheel repo (workspace/worktree checks no-op). Both throwaway dirs
#    live under the real $HOME rather than $work: $work is under a macOS
#    temp dir (mktemp's default), which the workspace-mount check itself
#    correctly flags as unshareable — using it here would manufacture a
#    real SeverityError and break this test's determinism. Overriding
#    HOME can also hide the docker CLI's context (colima/OrbStack keep
#    the socket path in ~/.docker/config.json, not the /var/run/docker.sock
#    default), so DOCKER_HOST is pinned from the current context first.
#    The only findings left are the advisory ones we just hid — full
#    doctor must still exit 0 and print them as warnings.
docker_host="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)}"
warn_root="$(mktemp -d "${HOME:-.}/.flywheel-doctor-scenario.XXXXXX")"
trap 'rm -rf "$work" "$warn_root"' EXIT
warn_farm="$warn_root/bin"
mkdir -p "$warn_farm"
for b in git k3d docker mkcert; do
  ln -s "$(command -v "$b")" "$warn_farm/$b"
done
warn_home="$warn_root/home"
warn_cwd="$warn_root/cwd"
mkdir -p "$warn_home" "$warn_cwd"
warn_out="$work/warn.txt"
if ! (cd "$warn_cwd" && PATH="$warn_farm" HOME="$warn_home" DOCKER_HOST="$docker_host" "$FW" doctor) >"$warn_out" 2>&1; then
  log "FAIL: full 'doctor' exited non-zero with only advisory tooling missing (pre-commit/yq/certutil)"
  cat "$warn_out"
  exit 1
fi
if ! grep -qi 'WARN' "$warn_out"; then
  log "FAIL: full 'doctor' printed no WARN for the missing advisory tooling"
  cat "$warn_out"
  exit 1
fi
log "doctor (full) warn path OK: advisory findings printed, exit 0 (T25)"

log "scenario doctor PASS"
