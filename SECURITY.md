# Security policy

Sentinel Watchdog runs as root and can restart services and change the
host firewall. We take vulnerabilities in it seriously.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub:
[Security → Report a vulnerability](https://github.com/sentinel-watchdog/sentinel-watchdog/security/advisories/new).

Please include:

- the affected version or commit;
- the configuration needed to reproduce (with secrets removed);
- the impact you expect (privilege escalation, firewall bypass, lockout,
  secret disclosure, denial of service, …) and how you found it.

What to expect — this is a project maintained in spare time, so these are
targets, not guarantees:

- acknowledgement within **7 days**;
- an assessment and a plan within **30 days**;
- coordinated disclosure: a fix and a GitHub security advisory (with a
  CVE when applicable) before details are made public, normally within
  **90 days** of the report. Reporters are credited unless they prefer
  otherwise.

## Supported versions

No release has been published yet. From v1.0.0, security fixes are made
for the latest minor release of the current major version.

## Scope

In scope: `sentineld`, `sentinelctl`, the configuration loader, the
control socket and its authorization, the state and audit files, the
release artifacts and packages, and this repository's workflows.

Out of scope: vulnerabilities in the software Sentinel supervises or
integrates with (systemd, nftables, CrowdSec, Docker, Kubernetes, …) —
report those upstream — and configurations that explicitly opt out of a
safety default (for example `tls.insecure_skip_verify: true`).

The threat model is in [docs/threat-model.md](docs/threat-model.md).
