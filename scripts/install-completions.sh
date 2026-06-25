#!/usr/bin/env bash
# Install the flywheel shell-completion script. With no argument, installs for
# the current shell ($SHELL); with `all`, installs for bash, zsh, and fish.
# Cleanly replaces any previous copy — writing to each shell's canonical path
# with `>` overwrites, so re-running is idempotent. Invoked by `make install` /
# `make completions` / `make completions-all`.
#
# Uses $FLYWHEEL (the just-built binary path passed by the Makefile) so it works
# even before GOBIN is on $PATH.
set -euo pipefail

FLYWHEEL="${FLYWHEEL:-flywheel}"
if [ ! -x "$FLYWHEEL" ] && ! command -v "$FLYWHEEL" >/dev/null 2>&1; then
	echo "error: flywheel binary not found ($FLYWHEEL); run 'make build' first" >&2
	exit 1
fi

install_for() {
	local shell="$1" dir dest
	case "$shell" in
	zsh)
		# zsh autoloads completions from a directory named on $fpath, as `_<cmd>`.
		dir="${HOME}/.zsh/completions"
		dest="${dir}/_flywheel"
		mkdir -p "$dir"
		"$FLYWHEEL" completion zsh >"$dest"
		echo "installed zsh completion → $dest"
		echo "  if it doesn't work, add to ~/.zshrc once: fpath=($dir \$fpath); autoload -Uz compinit && compinit"
		;;
	bash)
		dir="${HOME}/.local/share/bash-completion/completions"
		dest="${dir}/flywheel"
		mkdir -p "$dir"
		"$FLYWHEEL" completion bash >"$dest"
		echo "installed bash completion → $dest (needs bash-completion enabled; restart shell)"
		;;
	fish)
		dir="${HOME}/.config/fish/completions"
		dest="${dir}/flywheel.fish"
		mkdir -p "$dir"
		"$FLYWHEEL" completion fish >"$dest"
		echo "installed fish completion → $dest (fish autoloads it; restart shell)"
		;;
	*)
		echo "warning: unrecognised shell '${shell:-<unset>}' (\$SHELL); install manually, e.g.:" >&2
		echo "  $FLYWHEEL completion zsh > ~/.zsh/completions/_flywheel" >&2
		return 1
		;;
	esac
}

if [ "${1:-}" = "all" ]; then
	for s in bash zsh fish; do install_for "$s" || true; done
else
	install_for "$(basename "${SHELL:-}")" || true
fi
