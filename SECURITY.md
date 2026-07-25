# Security Policy

microvm exists to run untrusted code inside Firecracker microVMs. Isolation is
the whole point of the project, so security reports are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.** Report it
privately through GitHub's
[private vulnerability reporting](https://github.com/pablofdezr/microvm/security/advisories/new).

Include enough detail to reproduce: the affected version or commit, the relevant
configuration (how tokens are supplied, storage, networking, per-tenant limits),
and a proof of concept if you have one.

## Scope

Of particular interest:

- **Guest-to-host escapes** or any breach of VM isolation.
- **Network firewall bypasses** — a sandbox reaching RFC1918, link-local, or the
  cloud metadata endpoint at `169.254.169.254`.
- **SSRF through the source fetcher** — anything that makes the daemon connect
  somewhere its policy should have refused when seeding a sandbox from a caller's
  `source` URL (`-source-fetch`). The daemon fetches from *outside* the firewall it
  installs for guests, and the caller reads the result out of their own sandbox, so
  this reaches the host's LAN, the API on loopback, and the metadata service that
  holds the host's identity. In scope: getting past the `-source-allow-host`
  allowlist (a redirect, a name that resolves oddly, a URL parsed differently by the
  check and the connect), reaching a blocked address (DNS rebinding between the
  check and the dial, an IPv4-mapped or NAT64 form, a scheme other than `https`), or
  making an operator credential leak into a guest, a log, a URL or `argv`. Also in
  scope: an archive or checkout that writes outside the guest's working directory,
  or past `-source-max-*`.
- **Auth bypasses** — defeating bearer-token auth or crossing a tenant boundary.
- **Metering, quota or admission bypasses** — escaping per-tenant storage
  quotas, usage accounting, or the per-tenant sandbox and request-rate limits
  (`-tenant-max-sandboxes` / `-tenant-max-rps`).
- **Resource exhaustion reachable from one authenticated request** — a body inside
  the documented size cap that costs the daemon disproportionately more than it.

One known limitation, so it need not be reported: `-tenant-max-sandboxes` counts
sandboxes on the node that holds the count, and a task submitted to
`POST /v1/tasks` is scheduled across the fleet, so task-backed sandboxes are not
charged to their submitter. Bound task load with `-slots`, `-cpu` and `-mem`. See
DEPLOY.md.

Reports that require an already-compromised host, or that depend on running the
daemon with auth disabled (no tokens configured) on an exposed host, are out of
scope: that configuration is documented as unsafe.
