# Security Policy

## Supported versions

Flywheel is under active development (v0.x). Security fixes land in the **latest
release** — there are no backports to older `v0.x` tags. Always run the most
recent release (`flywheel version` to check yours).

| Version | Supported |
|---------|-----------|
| latest `v0.x` release | :white_check_mark: |
| older `v0.x` tags | :x: |

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Instead, use GitHub's private vulnerability reporting:

1. Open the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Fill in the advisory form.

This opens a private channel visible only to the maintainers. If private
reporting is unavailable, open a *minimal* public issue that asks a maintainer
to set up a private channel — **do not include any vulnerability details** in
it.

Please include where you can:

- the Flywheel version (`flywheel version`),
- your OS and docker runtime (Docker Desktop / Colima / podman),
- a clear description and a minimal reproduction,
- the impact you've identified.

## Response

Flywheel is a small project under active v0.x development. We triage reports on
a best-effort basis and aim to acknowledge within a few business days, then keep
you updated as we investigate and coordinate a fix and disclosure.

## Scope notes

Flywheel is a local-first developer tool. A few things that look secret-ish are
intentional and are **not** vulnerabilities:

- **The committed `clusters/local/age.key`** in a generated GitOps repo. It only
  ever decrypts `clusters/local/*` dev secrets on your localhost k3d cluster;
  every other environment's key stays out of git. See the onboarding guide.
- **Anonymous push to the in-cluster git-server.** It is reachable only from
  inside the local cluster, never exposed via Ingress.

If you're unsure whether something is in scope, report it privately and we'll
help figure it out.
