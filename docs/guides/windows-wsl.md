# Windows (WSL)

There is no native Windows build — run Flywheel inside a **WSL2** Linux distro
(it uses the `linux/amd64`/`arm64` binary). It works there, with three
WSL-specific things to get right:

- **Clone into the distro's own filesystem, not `/mnt/c`.** `flywheel up`
  bind-mounts the parent of your repo (`paths.workspaces_root`) into every k3d
  node. On a Windows drive (`/mnt/c/...`, a 9p mount) git operations are slow
  and hit ownership/permission quirks; keep your repos under `~` inside the
  distro (e.g. `~/src/...`).

- **Docker daemon.** k3d needs one. Either enable Docker Desktop's *WSL
  integration* for your distro, or run a native `dockerd` inside it. `flywheel
  doctor` pings the daemon; if it can't reach one, `up` won't start.

- **mkcert trust is split between WSL and Windows.** `flywheel up` runs `mkcert
  -install`, which only touches the **Linux** trust store inside WSL. For that
  to cover Firefox/Chromium in WSL you also need `certutil` (`sudo apt install
  libnss3-tools`) — `flywheel doctor` flags it when missing. But if you browse
  from a **Windows** browser, import the same CA into Windows too: run
  `cp "$(mkcert -CAROOT)/rootCA.pem" /mnt/c/Users/<you>/` and import it into
  *Certificates → Trusted Root Certification Authorities* (or run mkcert on the
  Windows side against the same `$CAROOT`). Otherwise apps load over HTTPS with
  a certificate warning.

For reaching `*.localdev.me` in the browser, see the [Local DNS guide](local-dns.md)
— on WSL a Windows browser resolves via the **Windows** hosts file
(`C:\Windows\System32\drivers\etc\hosts`, edited as Administrator), not WSL's
`/etc/hosts`. The public `localdev.me` wildcard already points at `127.0.0.1`,
so on a network whose resolver doesn't strip private answers you often need no
hosts entry at all.
