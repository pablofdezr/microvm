# Security Policy

microvm exists to run untrusted code inside Firecracker microVMs. Isolation is
the whole point of the project, so security reports are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.** Report it
privately through GitHub's
[private vulnerability reporting](https://github.com/pablofdezr/microvm/security/advisories/new).

Include enough detail to reproduce: the affected version or commit, the relevant
configuration (flags such as `-tokens` / `-admin-tokens`, storage, networking),
and a proof of concept if you have one.

## Scope

Of particular interest:

- **Guest-to-host escapes** or any breach of VM isolation.
- **Network firewall bypasses** — a sandbox reaching RFC1918, link-local, or the
  cloud metadata endpoint at `169.254.169.254`.
- **Auth bypasses** — defeating bearer-token auth (`-tokens` / `-admin-tokens`)
  or crossing a tenant boundary.
- **Metering or quota bypasses** — escaping per-tenant storage quotas or usage
  accounting.

Reports that require an already-compromised host, or that depend on running the
daemon with auth disabled (empty `-tokens`) on an exposed host, are out of scope:
that configuration is documented as unsafe.
