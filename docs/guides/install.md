# Installing & uninstalling

The one-liner is in the [README](../../README.md#installation). This guide
covers pinning a version, sudo-less installs, building from source, and
uninstalling.

## Install script

`install.sh` downloads a prebuilt binary for your OS/arch (darwin/linux ×
amd64/arm64) from the
[latest release](https://github.com/cobr-io/flywheel/releases), verifies its
checksum, and installs it on your `$PATH`:

```sh
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | bash
```

Re-run it to upgrade; it no-ops when the target version is already installed.
It does not auto-update: each repo pins its release in `flywheel.yaml`, and the
binary should track that pin rather than float ahead of it (see
[Upgrading](upgrading.md)).

It also installs shell tab-completions for your login shell (`$SHELL`) into
that shell's canonical autoload dir. This is best-effort: it warns and
continues if the dir isn't writable. Restart your shell to pick them up.

Options are environment variables on the `bash` side of the pipe, not the
`curl` side:

| Variable | Default | Effect |
|---|---|---|
| `TAG` | latest | Install a specific release, e.g. `TAG=v1.2.3`. |
| `INSTALL_DIR` | `/usr/local/bin` | Where to put the binary. |
| `USE_SUDO` | auto | `sudo` is used only when `INSTALL_DIR` isn't writable; set `false` to never elevate (pair with a writable `INSTALL_DIR`). |
| `FORCE` | `false` | Reinstall even when the target version is already present. |
| `SKIP_COMPLETIONS` | `false` | Set `true` to skip installing shell tab-completions. |

```sh
# pin a specific version
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | TAG=v1.2.3 bash

# user-local install, no sudo
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh \
  | INSTALL_DIR="$HOME/.local/bin" USE_SUDO=false bash
```

There is no native Windows build; run inside WSL2
([guide](windows-wsl.md)). A Homebrew tap is planned.

## From source

Requires the Go toolchain (see [`go.mod`](../../go.mod)) and the `docker` CLI.
From a checkout:

```sh
make install      # version-stamped binary + runtime images + shell completions
make build        # just the binary
```

`make install` installs the binary into `$(go env GOBIN)` (put it on your
`$PATH`) and builds the four runtime images locally for
[dogfood mode](../dev/dogfood.md). You can also
`go install github.com/cobr-io/flywheel/cmd/flywheel@vX.Y.Z`, but that binary
is stamped `v0.0.0-dev` and skips the version-drift check `up` normally runs.

## Uninstall

By default only the binary and the shell completions are removed. Caches and
config are left alone, because `~/.config/flywheel` holds age private keys
that are recovery-critical (see the caution below).

```sh
# undo an install-script install (same INSTALL_DIR / USE_SUDO overrides apply)
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash

# undo a `make install` (removes $(go env GOBIN)/flywheel + completions)
make uninstall
```

Two opt-in cleanup flags:

| Flag | Effect |
|---|---|
| `--purge` | Also remove the embed cache `~/.cache/flywheel` (regenerated on the next `init`/`up`). |
| `--purge-config` | Also remove `~/.config/flywheel` entirely, including age keys and per-cluster state. Destructive and irreversible. |

```sh
# binary + completions + embed cache, but keep age keys/config
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh | bash -s -- --purge

# a user-local install lives elsewhere — point INSTALL_DIR at it
curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/uninstall.sh \
  | INSTALL_DIR="$HOME/.local/bin" USE_SUDO=false bash
```

> **Caution: `--purge-config` deletes your age keys.**
> `~/.config/flywheel/<name>/age.key` is the private key that decrypts your
> SOPS-encrypted state. Deleting it can make that state permanently
> unrecoverable. A plain uninstall never touches `~/.config/flywheel`; only
> `--purge-config` does, and it warns loudly first. Back up your keys before
> using it if you might still need any encrypted secrets.
