# Security policy

## Supported versions

Only the latest release receives security fixes. Upgrade to the newest
`ghcr.io/connor-ryan-1998/keiryo` tag or release archive before reporting.

## Reporting a vulnerability

Please **do not open a public issue** for security problems.
Report privately through GitHub's
[private vulnerability reporting](https://github.com/Connor-Ryan-1998/keiryo/security/advisories/new).

Include the affected version, configuration (auth mode, proxy setup), steps to
reproduce and the impact you observed. You should get an acknowledgement within
7 days. Please allow time for a fix and release before public disclosure.

## Scope and known trust boundaries

These are documented design properties, not vulnerabilities:

- Docker socket access is effectively host-root access, even when mounted read-only.
- The agent reads host filesystems and runs as root in the provided Compose example.
- The hub has no built-in TLS; remote traffic must use HTTPS via a reverse proxy or a private encrypted network.
- Proxy auth mode trusts identity headers from the configured proxy address only.

See [authentication and network boundary](docs/reference.md#authentication-and-network-boundary)
for details. Bypasses of these boundaries (for example, spoofing proxy identity
from an untrusted address, or one host's token submitting another host's data)
are in scope.
