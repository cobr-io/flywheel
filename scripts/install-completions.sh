#!/usr/bin/env bash
# Install the flywheel shell-completion script. With no argument, installs for
# the current shell ($SHELL); with `all`, installs for bash, zsh, and fish.
# Cleanly replaces any previous copy — writing to each shell's canonical path
# with `>` overwrites, so re-running is idempotent. Invoked by `make install` /
# `make completions` / `make completions-all`.
#
# Uses $FLYWHEEL (the just-built binary path passed by the Makefile) so it works
# even before GOBIN is on $PATH.
#
# This script is the single source of truth for the three completion
# destination paths (zsh/bash/fish) — see `dest_for` below. `--print-paths`
# emits them as `<shell> <dest>` lines, one per shell, without needing a
# flywheel binary. Consumers:
#   - uninstall.sh sources this via --print-paths when available (falling
#     back to a static copy of the same paths for standalone/old installs).
#   - install.sh can't source this (it's fetched standalone via curl, before
#     any repo checkout exists), so it inlines the same paths by design.
#   - scripts/check-completion-paths-drift.sh (run in CI) asserts install.sh
#     and uninstall.sh's copies never drift from this file.
set -euo pipefail

# dest_for <shell> — echoes the canonical completion-file destination for a
# shell, or returns 1 for an unrecognised shell.
dest_for() {
	case "$1" in
	zsh)  printf '%s\n' "${HOME}/.zsh/completions/_flywheel" ;;
	bash) printf '%s\n' "${HOME}/.local/share/bash-completion/completions/flywheel" ;;
	fish) printf '%s\n' "${HOME}/.config/fish/completions/flywheel.fish" ;;
	*) return 1 ;;
	esac
}

if [ "${1:-}" = "--print-paths" ]; then
	for s in zsh bash fish; do
		printf '%s %s\n' "$s" "$(dest_for "$s")"
	done
	exit 0
fi

FLYWHEEL="${FLYWHEEL:-flywheel}"
if [ ! -x "$FLYWHEEL" ] && ! command -v "$FLYWHEEL" >/dev/null 2>&1; then
	echo "error: flywheel binary not found ($FLYWHEEL); run 'make build' first" >&2
	exit 1
fi

install_for() {
	local shell="$1" dir dest
	dest="$(dest_for "$shell")" || {
		echo "warning: unrecognised shell '${shell:-<unset>}' (\$SHELL); install manually, e.g.:" >&2
		echo "  $FLYWHEEL completion zsh > ~/.zsh/completions/_flywheel" >&2
		return 1
	}
	dir="$(dirname "$dest")"
	mkdir -p "$dir"
	"$FLYWHEEL" completion "$shell" >"$dest"
	case "$shell" in
	zsh)
		echo "installed zsh completion → $dest"
		echo "  if it doesn't work, add to ~/.zshrc once: fpath=($dir \$fpath); autoload -Uz compinit && compinit"
		;;
	bash)
		echo "installed bash completion → $dest (needs bash-completion enabled; restart shell)"
		;;
	fish)
		echo "installed fish completion → $dest (fish autoloads it; restart shell)"
		;;
	esac
}

if [ "${1:-}" = "all" ]; then
	for s in bash zsh fish; do install_for "$s" || true; done
else
	install_for "$(basename "${SHELL:-}")" || true
fi
