# Install

How to get, verify, install, upgrade and remove Sentinel Watchdog. Running
it is in [operations](operations.md), configuring it in
[configuration](configuration.md).

**No release is published yet**: the first one is v0.3.1 (Phase 3d-3), and
the tags before it have no files. Until then, use a [snapshot](#snapshots).

## The files of a release

Each release has, for `amd64` (`x86_64`) and `arm64` (`aarch64`):

| File | Content |
|---|---|
| `sentinel-watchdog_<version>_<arch>.deb`, `sentinel-watchdog-<version>-1.<arch>.rpm`, `sentinel-watchdog_<version>_<arch>.apk` | packages: the binaries in `/usr/bin`, the systemd unit (deb, rpm) or the OpenRC script (apk), `/etc/sentinel/sentinel.yaml`, documentation and examples in `/usr/share/doc/sentinel-watchdog/` |
| `sentinel-watchdog_<version>_linux_<arch>.tar.gz` | `sentineld`, `sentinelctl`, `LICENSE`, `README.md`, `CHANGELOG.md`, `examples/sentinel.yaml`, `examples/supervisor/http.yaml`, `examples/supervisor/process.yaml`, `examples/supervisor/systemd.yaml`, `examples/supervisor/jobs.yaml`, `deploy/systemd/sentineld.service`, `deploy/openrc/sentineld` |
| `<binary>_<version>_linux_<arch>.cdx.json` | CycloneDX SBOM of each binary: the Go modules and standard library linked into it |
| `checksums.txt` | SHA-256 of every file above |
| `checksums.txt.sigstore.json` | release only: the signature of `checksums.txt` (Sigstore bundle) |

Binaries are static (`CGO_ENABLED=0`): one build runs on glibc and musl
(Alpine) systems. Builds are reproducible: the same commit gives the same
bytes (`task reproduce` checks it).

## Verify a release

Packages carry no signature of their own (D-102). What is signed is
`checksums.txt`, by the release workflow of this repository, with no
long-lived key: the signature names the workflow and the tag it ran for
(D-103). So verify in this order, before installing anything:

1. the signature of `checksums.txt`, which says who made the list;
2. the checksum of your file, which says the file is the one in the list.

You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
3, the version that writes the bundle. Work as root in a directory only
root can write (`cd "$(mktemp -d)"`): in a shared one, such as a user's
home, somebody could change the file between the check and the installation.
Download there, or copy (a moved file may still be open elsewhere), your file, `checksums.txt` and `checksums.txt.sigstore.json` from
the [release page](https://github.com/sentinel-watchdog/sentinel-watchdog/releases),
and say what you are checking:

```sh
tag=v0.3.1                                 # the release you downloaded
f=sentinel-watchdog_0.3.1_amd64.deb        # the file you downloaded
repo=sentinel-watchdog/sentinel-watchdog
```

Then:

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt &&
awk -v f="$f" '$2 == f' checksums.txt > "$f.sha256" &&
[ "$(wc -l < "$f.sha256")" -eq 1 ] && sha256sum -c "$f.sha256"
```

It must print `Verified OK` and then `<file>: OK`. Anything else, or
nothing: do not install the file. Keep the identity exact: a pattern would
also accept other workflows or other tags, such as an older release.

`checksums.txt` and every file it lists also have build provenance, a
signed statement of the workflow, commit and tag that built them. With
[gh](https://cli.github.com) (logged in), instead of the two steps above or
after them, with the same variables:

```sh
gh attestation verify "$f" --repo "$repo" \
  --signer-workflow "$repo/.github/workflows/release.yml" \
  --source-ref "refs/tags/$tag"
```

Both vouch for the workflow and the tag. They do not cover a build tool
compromised while it ran in that workflow (D-103).

## Install a package

After the verification, as root, in the directory of the file:

| System | Command (amd64; use your architecture's file) |
|---|---|
| Debian, Ubuntu | `apt install ./sentinel-watchdog_<version>_amd64.deb` |
| RHEL, Rocky, Alma | `dnf install ./sentinel-watchdog-<version>-1.x86_64.rpm` |
| openSUSE | `zypper install --allow-unsigned-rpm ./sentinel-watchdog-<version>-1.x86_64.rpm` |
| Alpine | `apk add --allow-untrusted ./sentinel-watchdog_<version>_x86_64.apk` |

`--allow-unsigned-rpm` and `--allow-untrusted` skip the package manager's
own signature check, which these packages cannot pass: use them only on a
file you have just verified.

sentineld is installed but **not** enabled or started. Edit
`/etc/sentinel/sentinel.yaml`, check it, then start it:

```sh
sentineld -validate -config /etc/sentinel/sentinel.yaml
systemctl enable --now sentineld                                   # systemd
rc-update add sentineld default && rc-service sentineld start      # Alpine
sentinelctl status
```

## What the package does

- **Install** creates the system group `sentinel` (members may read the
  status), `/etc/sentinel/` (0750), `/etc/sentinel/sentinel.yaml` (0640,
  no module enabled) and `/var/lib/sentinel/` (0750). It does **not**
  enable or start sentineld (above).
- It **refuses** to install when `/etc/sentinel` or `/var/lib/sentinel`
  already exists as a symlink, as something other than a directory, not
  owned by root or writable by group or others, or when `/etc`, `/var` or
  `/var/lib` is not root's alone, and names the path to fix. On Alpine,
  `apk add` reports that error but installs the package anyway (apk-tools
  2.14 cannot stop a transaction from a script); sentineld's own checks
  then apply at start (a directory others can write is refused). Remove the
  package, fix the path, install again.
- An upgrade or removal of the package must not run as a job or process
  that sentineld supervises: restarting or stopping sentineld stops that
  job, and with it the package manager.
- **Upgrade** keeps your `sentinel.yaml` (the new default lands beside it as
  `.dpkg-dist`, `.rpmnew` or `.apk-new`) and restarts sentineld only if it
  runs; a restart also restarts the processes it supervises. If the new
  version rejects your configuration, the upgrade still completes, prints
  that sentineld did not restart, and the service stays down until you fix
  it: check with `sentineld -validate -config /etc/sentinel/sentinel.yaml`,
  then `systemctl reset-failed sentineld` (systemd stops retrying after a
  few failed starts) and `systemctl restart sentineld`; on Alpine,
  `rc-service sentineld restart`.
- **Removal** stops and disables sentineld (a stop that fails, or one a
  `policy-rc.d` denies, stops the removal; on Alpine apk removes the
  package anyway, so stop sentineld first: `rc-service sentineld stop`)
  and keeps `/var/lib/sentinel` (state and audit log), the group and a
  modified configuration (rpm renames it `.rpmsave`). **Purge**
  (`apt purge`, `apk del --purge`) also deletes `/etc/sentinel/sentinel.yaml`,
  even modified. Nothing ever deletes `/var/lib/sentinel`: remove it by
  hand (`rm -rf /var/lib/sentinel`) once you no longer need its audit log.

## Tarball

The tarball is for systems without one of these package managers. After
the verification, with `f` set to the tarball, as root:

```sh
d=$(mktemp -d) && tar -xzf "$f" -C "$d" &&
  install -o root -g root -m 0755 "$d"/*/sentineld "$d"/*/sentinelctl /usr/local/bin/ &&
  sentineld -version
```

The tarball carries the packaged unit and OpenRC script in `deploy/`;
they name `/usr/bin`, so a tarball install overrides the paths: a
[drop-in](operations.md#run-under-systemd) for systemd, a copy of the script
for OpenRC. Create `/etc/sentinel` (root,
0750) and the groups your configuration names yourself.

## Snapshots

`task snapshot` (or the `snapshot` artifact of a CI run) produces the same
files in `dist/`, without a signature or provenance. `<version>` is the tag
on a tagged commit, otherwise the next patch with `-pre<commits since the
tag>` (`0.3.1~pre5` for deb and rpm, `0.3.1_pre5` for apk), so every
package manager sees a newer snapshot as an upgrade, and the release as an
upgrade from its snapshots.

A checksum alone proves the file is the one the build produced, not who
built it: build a snapshot yourself, or take it from a CI run of a commit
you have reviewed. Check and install it as above, without the cosign step:

```sh
f=sentinel-watchdog_<version>_amd64.deb
awk -v f="$f" '$2 == f' checksums.txt > "$f.sha256"
[ "$(wc -l < "$f.sha256")" -eq 1 ] && sha256sum -c "$f.sha256" && apt install "./$f"
```
