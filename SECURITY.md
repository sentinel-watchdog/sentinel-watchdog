# Security policy

Sentinel Watchdog runs as root: it starts programs, restarts services and
will change the host firewall, so a bug in it can be a root bug. Please
report vulnerabilities privately, as below.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub:
[Security → Report a vulnerability](https://github.com/sentinel-watchdog/sentinel-watchdog/security/advisories/new).

Please include:

- the affected version or commit;
- the configuration needed to reproduce (with secrets removed);
- the impact you expect (privilege escalation, firewall bypass, lockout,
  secret disclosure, denial of service, …) and how you found it.

The project is maintained in spare time, so these are targets, not
guarantees:

- acknowledgement within **7 days**;
- an assessment and a plan within **30 days**;
- coordinated disclosure: a fix and a GitHub security advisory (with a
  CVE when applicable) before details are made public, normally within
  **90 days** of the report. Reporters are credited unless they prefer
  otherwise.

## Supported versions

The v0.x versions are previews (releases with files from v0.3.1, tags
alone before it) and get no fixes of their own; fixes go to `main`. From
v1.0.0, security fixes are made for the latest minor release of the
current major version.

## Scope

In scope: `sentineld`, `sentinelctl`, the configuration loader, the
control socket and its authorization, the state and audit files, the
supervisor's running of programs, its calls to systemd and its handling
of logs, the release artifacts and packages, and this repository's
workflows.

Out of scope: vulnerabilities in the software Sentinel supervises or
integrates with (systemd, nftables, CrowdSec, Docker, Kubernetes, …),
which belong upstream, and configurations that explicitly opt out of a
safety default (for example `tls.insecure_skip_verify: true`).

How to verify the signature of a release is in
[docs/install.md](docs/install.md#verify-a-release).

The threat model is in [docs/threat-model.md](docs/threat-model.md).
