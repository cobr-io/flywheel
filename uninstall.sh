#!/usr/bin/env bash
#
# Flywheel uninstaller — the inverse of install.sh.
#
#   curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash
#
# By default it removes only what install.sh / `make install` put on your
# machine: the binary and the shell-completion files. Caches and config are
# LEFT ALONE unless you explicitly ask for them — because ~/.config/flywheel
# holds age private keys that are recovery-critical: deleting them can make
# SOPS-encrypted state permanently unrecoverable.
#
# Environment overrides (mirroring install.sh):
#   INSTALL_DIR=DIR  where the binary lives   (default: /usr/local/bin)
#   USE_SUDO=false   never use sudo           (default: auto — sudo only if
#                                              INSTALL_DIR isn't writable)
#
# Flags:
#   --purge          also remove the embed cache (~/.cache/flywheel)
#   --purge-config   also remove ~/.config/flywheel ENTIRELY — INCLUDING age
#                    keys and per-cluster state. DESTRUCTIVE and IRREVERSIBLE.
#   -h, --help       show this help and exit
#
set -euo pipefail

BINARY="flywheel"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
USE_SUDO="${USE_SUDO:-auto}"
PURGE_CACHE=false
PURGE_CONFIG=false

# ---- logging (color only on a TTY) ----------------------------------------
if [ -t 2 ]; then
  BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m'); GREEN=$(printf '\033[32m')
  YELLOW=$(printf '\033[33m'); DIM=$(printf '\033[2m'); RESET=$(printf '\033[0m')
else
  BOLD=""; RED=""; GREEN=""; YELLOW=""; DIM=""; RESET=""
fi
info() { printf '%s %s\n' "${BOLD}flywheel${RESET}" "$*" >&2; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'EOF'
flywheel uninstaller — the inverse of install.sh

Usage: uninstall.sh [--purge] [--purge-config]
   or: curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash

Removes the flywheel binary and shell completions by default. Caches and config
are left alone unless you ask for them.

Environment:
  INSTALL_DIR=DIR   where the binary lives   (default: /usr/local/bin)
  USE_SUDO=false    never use sudo           (default: auto)

Flags:
  --purge           also remove the embed cache (~/.cache/flywheel)
  --purge-config    also remove ~/.config/flywheel ENTIRELY, INCLUDING age
                    keys and per-cluster state — DESTRUCTIVE, IRREVERSIBLE
  -h, --help        show this help and exit
EOF
  exit "${1:-0}"
}

# ---- args -----------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --purge)        PURGE_CACHE=true ;;
    --purge-config) PURGE_CONFIG=true ;;
    -h|--help)      usage 0 ;;
    *)              warn "unknown argument: $1"; usage 1 ;;
  esac
  shift
done

# ---- remove (sudo only when needed) ---------------------------------------
# Mirrors install.sh's run(): elevate only if INSTALL_DIR isn't writable.
run() {
  if [ "$USE_SUDO" = "false" ]; then
    "$@"
  elif [ -w "$INSTALL_DIR" ]; then
    "$@"
  else
    command -v sudo >/dev/null 2>&1 || die \
      "cannot write ${INSTALL_DIR} and sudo is unavailable — re-run with INSTALL_DIR pointing at the dir that holds ${BINARY}"
    info "${DIM}elevating with sudo to modify ${INSTALL_DIR}${RESET}"
    sudo "$@"
  fi
}

# ---- binary ---------------------------------------------------------------
DEST="${INSTALL_DIR}/${BINARY}"
if [ -e "$DEST" ] || [ -L "$DEST" ]; then
  run rm -f "$DEST"
  info "${GREEN}removed${RESET} ${DEST}"
else
  info "no binary at ${DEST} — skipping"
  if command -v "$BINARY" >/dev/null 2>&1; then
    other="$(command -v "$BINARY")"
    warn "another '${BINARY}' is still on your PATH at ${other}"
    warn "  re-run with INSTALL_DIR=\"$(dirname "$other")\" to remove it"
  fi
fi

# ---- shell completions ----------------------------------------------------
# scripts/install-completions.sh is the single source of truth for the three
# completion destination paths (--print-paths). Consume it when this
# uninstall.sh is running from inside a flywheel checkout (`make uninstall`,
# or a cloned repo); fall back to a static copy of the same paths for the
# standalone curl-pipe-bash uninstall (no repo checkout available) or an old
# install. scripts/check-completion-paths-drift.sh (run in CI) asserts the
# fallback below never drifts from the real source.
remove_completion() {
  local shell="$1" dest="$2"
  if [ -e "$dest" ]; then
    rm -f "$dest" && info "removed ${shell} completion → ${dest}"
  fi
}

COMPLETIONS_SCRIPT="$(dirname "${BASH_SOURCE[0]:-$0}")/scripts/install-completions.sh"
if [ -f "$COMPLETIONS_SCRIPT" ]; then
  while read -r shell dest; do
    remove_completion "$shell" "$dest"
  done < <(bash "$COMPLETIONS_SCRIPT" --print-paths)
else
  remove_completion zsh  "${HOME}/.zsh/completions/_flywheel"
  remove_completion bash "${HOME}/.local/share/bash-completion/completions/flywheel"
  remove_completion fish "${HOME}/.config/fish/completions/flywheel.fish"
fi

# ---- optional: embed cache ------------------------------------------------
CACHE_DIR="${HOME}/.cache/flywheel"
if [ "$PURGE_CACHE" = true ]; then
  if [ -d "$CACHE_DIR" ]; then
    rm -rf "$CACHE_DIR"
    info "${GREEN}purged${RESET} embed cache → ${CACHE_DIR}"
  else
    info "no embed cache at ${CACHE_DIR} — skipping"
  fi
elif [ -d "$CACHE_DIR" ]; then
  info "${DIM}kept${RESET} embed cache ${CACHE_DIR} (pass --purge to remove)"
fi

# ---- optional: config + age keys (DESTRUCTIVE, opt-in only) ---------------
CONFIG_DIR="${HOME}/.config/flywheel"
if [ "$PURGE_CONFIG" = true ]; then
  if [ -d "$CONFIG_DIR" ]; then
    printf '\n' >&2
    warn "${BOLD}${RED}--purge-config: about to DELETE ${CONFIG_DIR} and everything in it.${RESET}"
    warn "${RED}This includes age private keys (${CONFIG_DIR}/<client>/age.key).${RESET}"
    warn "${RED}Without those keys, SOPS-encrypted state is UNRECOVERABLE.${RESET}"
    warn "${RED}Ensure you have backups / no longer need any encrypted secrets.${RESET}"
    rm -rf "$CONFIG_DIR"
    info "${GREEN}purged${RESET} config → ${CONFIG_DIR}"
  else
    info "no config at ${CONFIG_DIR} — skipping"
  fi
elif [ -d "$CONFIG_DIR" ]; then
  info "${DIM}kept${RESET} config ${CONFIG_DIR} — age keys & per-cluster state left intact"
  info "${DIM}      (pass --purge-config to delete them; this is irreversible)${RESET}"
fi

# ---- done -----------------------------------------------------------------
printf '\n' >&2
info "${GREEN}uninstall complete${RESET}"
if [ "$PURGE_CONFIG" != true ] && [ -d "$CONFIG_DIR" ]; then
  info "your age keys remain at ${CONFIG_DIR} (kept on purpose)."
fi
