#!/usr/bin/env bash
# install.sh / uninstall.sh must pick the right install dir (#69).
#
# Fresh installs go to ~/.local/bin (on PATH, no sudo). An existing install is
# upgraded in place, whatever the PATH order, so a re-run never leaves a stale
# binary behind or lets one shadow the new install. uninstall.sh removes the
# flywheel on PATH and warns when another one is still there.
#
# Each case runs the real scripts in a throwaway HOME. `curl` is stubbed to
# serve a fake release tarball, `sudo` to record that it ran, and `install` to
# refuse any destination outside the sandbox.
# A read-only dir stands in for the root-owned /usr/local/bin, so run this as
# a non-root user. Run it locally exactly as CI does:
#   bash scripts/check-install-dir-resolution.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
REPO_ROOT="$PWD"

[ "$(id -u)" != "0" ] || { echo "FAIL: run as non-root (a read-only dir must stay unwritable)" >&2; exit 1; }

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

# fake_flywheel PATH VERSION: a binary that answers `version` like the real one.
fake_flywheel() {
  # shellcheck disable=SC2016  # $1 belongs to the generated script
  printf '#!/bin/sh\n[ "$1" = version ] && echo "flywheel v%s"\nexit 0\n' "$2" >"$1"
  chmod +x "$1"
}

# sandbox: fresh HOME, stubs, a read-only "system" bin dir and a release fixture.
sandbox() {
  SB="$(mktemp -d)"
  export HOME="$SB/home"; mkdir -p "$HOME"
  STUBS="$SB/stubs"; SYS="$SB/sys"; mkdir -p "$STUBS" "$SYS" "$SB/rel"

  fake_flywheel "$SB/rel/flywheel" 9.9.9
  tar -czf "$SB/release.tar.gz" -C "$SB/rel" flywheel

  cat >"$STUBS/curl" <<EOF
#!/bin/sh
out=""; url=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o|-w) [ "\$1" = -o ] && out="\$2"; shift ;;
    -*) ;;
    *) url="\$1" ;;
  esac
  shift
done
case "\$url" in
  */releases/latest) printf '%s' "https://github.com/cobr-io/flywheel/releases/tag/v9.9.9" ;;
  *.tar.gz) cp "$SB/release.tar.gz" "\$out" ;;
  *) exit 22 ;;
esac
EOF
  cat >"$STUBS/sudo" <<EOF
#!/bin/sh
touch "$SB/sudo-called"
chmod u+w "$SYS"; "\$@"; rc=\$?; chmod a-w "$SYS"; exit \$rc
EOF
  # Guard: never let a regression write outside the sandbox (e.g. a fallback
  # to the real /usr/local/bin).
  cat >"$STUBS/install" <<EOF
#!/bin/sh
for dest; do :; done
case "\$dest" in "$SB"/*) exec /usr/bin/install "\$@" ;; esac
echo "stub install: refusing to write outside the sandbox: \$dest" >&2
exit 1
EOF
  chmod +x "$STUBS/curl" "$STUBS/sudo" "$STUBS/install"
}

seal()    { chmod a-w "$SYS"; }                     # make SYS "root-owned"
cleanup() { [ -n "${SB:-}" ] && chmod -R u+w "$SB" && rm -rf "$SB"; SB=""; }
trap cleanup EXIT

# run_script SCRIPT PATH-DIRS...: run with only the given dirs + system tools.
run_script() {
  local script="$1"; shift
  local path="$STUBS"
  for d in "$@"; do path="$path:$d"; done
  env -i HOME="$HOME" PATH="$path:/usr/bin:/bin" SKIP_COMPLETIONS=true \
    ${EXTRA_ENV:+$EXTRA_ENV} "$BASH" "$REPO_ROOT/$script" >"$SB/out" 2>&1 \
    || { cat "$SB/out" >&2; return 1; }
}

version_of() { "$1" version | awk '{print $2}'; }

# ---- fresh install: ~/.local/bin, no sudo ---------------------------------
sandbox
run_script install.sh "$HOME/.local/bin" "$SYS" || fail "fresh: install.sh exited non-zero"
[ "$(version_of "$HOME/.local/bin/flywheel" 2>/dev/null)" = v9.9.9 ] \
  || fail "fresh: expected v9.9.9 in ~/.local/bin"
[ ! -e "$SB/sudo-called" ] || fail "fresh: sudo was used"
cleanup

# ---- existing root-owned install: upgraded in place, in both PATH orders --
for order in sys-first user-first; do
  sandbox
  fake_flywheel "$SYS/flywheel" 0.0.1; seal
  if [ "$order" = sys-first ]; then dirs=("$SYS" "$HOME/.local/bin")
  else dirs=("$HOME/.local/bin" "$SYS"); fi
  run_script install.sh "${dirs[@]}" || fail "$order: install.sh exited non-zero"
  [ "$(version_of "$SYS/flywheel")" = v9.9.9 ] || fail "$order: existing binary not upgraded in place"
  [ ! -e "$HOME/.local/bin/flywheel" ] || fail "$order: second binary left in ~/.local/bin"
  [ -e "$SB/sudo-called" ] || fail "$order: expected sudo for the read-only dir"
  grep -q "sudo-less install" "$SB/out" || fail "$order: no opt-out hint printed"
  cleanup
done

# ---- explicit INSTALL_DIR that is shadowed: warns -------------------------
sandbox
fake_flywheel "$SYS/flywheel" 0.0.1; seal
EXTRA_ENV="INSTALL_DIR=$HOME/.local/bin" run_script install.sh "$SYS" "$HOME/.local/bin" \
  || fail "shadowed: install.sh exited non-zero"
grep -q "$SYS/flywheel comes first on your PATH" "$SB/out" || fail "shadowed: no shadowing warning"
cleanup

# ---- uninstall: default ~/.local/bin install, no override -----------------
sandbox
mkdir -p "$HOME/.local/bin"; fake_flywheel "$HOME/.local/bin/flywheel" 9.9.9
run_script uninstall.sh "$HOME/.local/bin" "$SYS" || fail "uninstall: exited non-zero"
[ ! -e "$HOME/.local/bin/flywheel" ] || fail "uninstall: ~/.local/bin/flywheel not removed"
cleanup

# ---- uninstall: warns when another flywheel remains on PATH ---------------
sandbox
mkdir -p "$HOME/.local/bin"; fake_flywheel "$HOME/.local/bin/flywheel" 9.9.9
fake_flywheel "$SYS/flywheel" 0.0.1
run_script uninstall.sh "$HOME/.local/bin" "$SYS" || fail "uninstall-two: exited non-zero"
grep -q "still on your PATH at $SYS/flywheel" "$SB/out" || fail "uninstall-two: no leftover warning"
cleanup

[ "$failures" -eq 0 ] || exit 1
echo "ok: install dir resolution"
