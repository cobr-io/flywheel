# Local DNS — reaching your apps in the browser

`flywheel up` tells you to visit `https://<app>.<domain>/`, where `<domain>` is
`local.domain` from `flywheel.yaml` (default `localdev.me`). Two things have to
line up for that to work in a browser:

* **Port.** The cluster publishes HTTPS on `cluster.https_port` (default
  `8540`), not `443`, so the URL is `https://<app>.<domain>:8540/`.
* **Name resolution.** `<app>.<domain>` must resolve to `127.0.0.1`.
  `localdev.me` is **not** a public wildcard resolver, so you provide the
  resolution locally. (`curl --resolve` and the e2e tests sidestep this; a real
  browser does not.)

The quick way is `/etc/hosts` — one line per app, no wildcard:

```sh
echo "127.0.0.1 hello.localdev.me" | sudo tee -a /etc/hosts
```

The durable way is **dnsmasq**, which resolves the whole `*.<domain>` wildcard
(every current and future app) to `127.0.0.1`. On macOS with Homebrew:

```sh
brew install dnsmasq

# Resolve the wildcard. The leading dot matches the apex + all subdomains.
echo 'address=/.localdev.me/127.0.0.1' >> "$(brew --prefix)/etc/dnsmasq.conf"

# Route *.localdev.me queries to dnsmasq. macOS reads one file per domain
# from /etc/resolver/ (root-owned, hence sudo).
echo 'nameserver 127.0.0.1' | sudo tee /etc/resolver/localdev.me

sudo brew services start dnsmasq          # binds privileged port 53 → run as root
sudo dscacheutil -flushcache
sudo killall -HUP mDNSResponder
```

Verify with `dscacheutil` or the browser — **not `dig`**, which queries the
upstream resolver directly and ignores `/etc/resolver`, so it keeps showing
nothing even when resolution works:

```sh
dscacheutil -q host -a name hello.localdev.me      # expect: ip_address: 127.0.0.1
```

**Non-standard dnsmasq port (gotcha).** If dnsmasq runs as a *user* service it
can't bind the privileged port 53, so it listens on something like `53535`
(`port=53535` in `dnsmasq.conf`). In that case the `/etc/resolver/<domain>` file
**must** name the same port, or queries go to `:53` where nothing answers and
you get `ERR_NAME_NOT_RESOLVED`:

```
nameserver 127.0.0.1
port 53535
```

Confirm dnsmasq itself is answering by querying it on its port directly (this
*does* bypass `/etc/resolver`, which is the point):
`dig @127.0.0.1 -p 53535 hello.localdev.me` should return `127.0.0.1`.

**TLS.** Flywheel serves a `*.localdev.me` cert signed by the mkcert local CA.
Run `mkcert -install` once so your browser/Keychain trusts it; otherwise the
page loads but shows a certificate warning.

On Linux, point `/etc/hosts` at the apps, or run dnsmasq via NetworkManager /
`systemd-resolved` with an equivalent `address=` rule.

On Windows (WSL), a Windows browser resolves via the **Windows** hosts file —
see the [Windows (WSL) guide](windows-wsl.md).
