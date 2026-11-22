#!/usr/bin/env bash
# Checks the packages in a dist/ directory against what D-102 says they
# hold (seam S2): file list, owners and modes, configuration files, which
# repository script is bound to which event, architecture (metadata and
# ELF), version and the fixed build host and time. The expected lists are
# written here by hand, not read from .goreleaser.yaml.
#
# Runs in the image of tests/packaging/Dockerfile (task package-verify).
# Usage: verify_packages.sh <repo dir> <dist dir>
# Environment: SENTINEL_PKG_VERSION, SOURCE_DATE_EPOCH (as Task sets them).
# Exit status: 0 every check passes, 1 a check failed, 2 usage.
set -euo pipefail

[[ $# -eq 2 ]] || {
	echo "usage: verify_packages.sh <repo dir> <dist dir>" >&2
	exit 2
}
readonly REPO=$1 DIST=$2
readonly VERSION=${SENTINEL_PKG_VERSION:?} EPOCH=${SOURCE_DATE_EPOCH:?}
readonly SCRIPTS=$REPO/packaging/scripts
readonly MAINTAINER="Marco Da Re <marco@mrcdare.dev>"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

fail() {
	echo "FAIL $*" >&2
	failures=$((failures + 1))
}

# Files of every package: "mode owner:group path", directories with a
# trailing slash. Parent directories such as /usr/ are left out.
readonly COMMON="drwxr-x--- root:root /etc/sentinel/
-rw-r----- root:root /etc/sentinel/sentinel.yaml
-rwxr-xr-x root:root /usr/bin/sentinelctl
-rwxr-xr-x root:root /usr/bin/sentineld
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/CHANGELOG.md
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/README.md
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/examples/sentinel.yaml
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/examples/supervisor/http.yaml
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/examples/supervisor/jobs.yaml
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/examples/supervisor/process.yaml
-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/examples/supervisor/systemd.yaml"
readonly SYSTEMD_UNIT="-rw-r--r-- root:root /usr/lib/systemd/system/sentineld.service"
readonly DEB_ONLY="-rw-r--r-- root:root /usr/share/doc/sentinel-watchdog/copyright
-rw-r--r-- root:root /usr/share/lintian/overrides/sentinel-watchdog
$SYSTEMD_UNIT"
readonly RPM_ONLY="-rw-r--r-- root:root /usr/share/licenses/sentinel-watchdog/LICENSE
$SYSTEMD_UNIT"
readonly APK_ONLY="-rwxr-xr-x root:root /etc/init.d/sentineld
-rw-r--r-- root:root /usr/share/licenses/sentinel-watchdog/LICENSE"

# Directories the packages may list without owning anything we check.
readonly SHARED_DIRS='^d[^ ]* root:root /(etc|etc/init\.d|usr|usr/bin|usr/lib|usr/lib/systemd|usr/lib/systemd/system|usr/share|usr/share/doc|usr/share/doc/sentinel-watchdog|usr/share/doc/sentinel-watchdog/examples|usr/share/doc/sentinel-watchdog/examples/supervisor|usr/share/licenses|usr/share/licenses/sentinel-watchdog|usr/share/lintian|usr/share/lintian/overrides)/$'

# compare_list <name> <expected> <actual file>
compare_list() {
	sort <<<"$2" >"$work/want"
	grep -Ev "$SHARED_DIRS" "$3" | sort >"$work/got"
	diff -u "$work/want" "$work/got" >"$work/diff" || fail "$1: file list differs:
$(cat "$work/diff")"
}

# same_script <name> <event> <repo script> <packaged script file>
same_script() {
	cmp -s "$3" "$4" || fail "$1: $2 is not ${3#"$REPO"/}"
}

# check_fs <name> <extracted root> <arch> <init: systemd | openrc>: the
# binaries are static ELF for the architecture, and the files a package
# installs from the repository are the repository's bytes.
check_fs() {
	local name=$1 root=$2 arch=$3 init=$4 machine=x86-64 bin
	case $arch in arm64 | aarch64) machine=aarch64 ;; esac
	for bin in sentineld sentinelctl; do
		file -b "$root/usr/bin/$bin" | grep -q "ELF 64-bit.*$machine.*statically linked" \
			|| fail "$name: $bin is $(file -b "$root/usr/bin/$bin")"
	done
	same_file "$name" "$REPO/packaging/sentinel.yaml" "$root/etc/sentinel/sentinel.yaml"
	case $init in
		systemd) same_file "$name" "$REPO/deploy/systemd/sentineld.service" "$root/usr/lib/systemd/system/sentineld.service" ;;
		openrc) same_file "$name" "$REPO/deploy/openrc/sentineld" "$root/etc/init.d/sentineld" ;;
	esac
}

same_file() { # <name> <repository file> <packaged file>
	cmp -s "$2" "$3" || fail "$1: ${3#"$work"/*/} is not ${2#"$REPO"/}"
}

# same_times <name> <tar -tv --full-time listing>: every member has the
# commit's time (the listing is in UTC: the image has no time zone).
same_times() {
	local want
	want=$(date -u -d "@$EPOCH" '+%Y-%m-%d %H:%M:%S')
	if grep -v -- " $want " "$2" | grep -q .; then
		fail "$1: members with another time than the commit's ($want): $(grep -v -- " $want " "$2" | head -3)"
	fi
}

check_deb() { # <file> <arch>
	local pkg=$1 arch=$2 name
	name=$(basename "$pkg")
	[[ $(dpkg-deb -f "$pkg" Package) == sentinel-watchdog ]] || fail "$name: package name"
	[[ $(dpkg-deb -f "$pkg" Maintainer) == "$MAINTAINER" ]] || fail "$name: maintainer $(dpkg-deb -f "$pkg" Maintainer)"
	[[ $(dpkg-deb -f "$pkg" Architecture) == "$arch" ]] || fail "$name: architecture $(dpkg-deb -f "$pkg" Architecture), want $arch"
	[[ $(dpkg-deb -f "$pkg" Version) == "${VERSION/-/\~}" ]] || fail "$name: version $(dpkg-deb -f "$pkg" Version)"
	dpkg-deb --fsys-tarfile "$pkg" | tar -tvf - --full-time >"$work/raw"
	dpkg-deb --fsys-tarfile "$pkg" | tar -tvf - \
		| awk '{p=$6; sub(/^\./, "", p); sub(/\/root/, ":root", $2); print $1, $2, p}' >"$work/list"
	compare_list "$name" "$COMMON
$DEB_ONLY" "$work/list"
	rm -rf "$work/ctl" && mkdir "$work/ctl"
	dpkg-deb -e "$pkg" "$work/ctl"
	[[ $(cat "$work/ctl/conffiles") == /etc/sentinel/sentinel.yaml ]] || fail "$name: conffiles: $(cat "$work/ctl/conffiles")"
	same_script "$name" preinst "$SCRIPTS/preinstall.sh" "$work/ctl/preinst"
	same_script "$name" postinst "$SCRIPTS/deb/postinst" "$work/ctl/postinst"
	same_script "$name" prerm "$SCRIPTS/deb/prerm" "$work/ctl/prerm"
	same_script "$name" postrm "$SCRIPTS/deb/postrm" "$work/ctl/postrm"
	rm -rf "$work/fs" && mkdir "$work/fs"
	dpkg-deb -x "$pkg" "$work/fs"
	check_fs "$name" "$work/fs" "$arch" systemd
	same_file "$name" "$REPO/packaging/debian/copyright" "$work/fs/usr/share/doc/sentinel-watchdog/copyright"
	same_file "$name" "$REPO/packaging/debian/lintian-overrides" "$work/fs/usr/share/lintian/overrides/sentinel-watchdog"
	same_times "$name" "$work/raw"
}

# rpm_script <file> <tag>: the body rpm stored for a scriptlet.
rpm_script() { rpm -qp --nosignature --qf "%{$2}" "$1"; }

check_rpm() { # <file> <arch>
	local pkg=$1 arch=$2 name want
	name=$(basename "$pkg")
	q() { rpm -qp --nosignature --qf "$1" "$pkg"; }
	[[ $(q '%{NAME}') == sentinel-watchdog ]] || fail "$name: package name"
	[[ $(q '%{ARCH}') == "$arch" ]] || fail "$name: architecture $(q '%{ARCH}'), want $arch"
	want=${VERSION/-/\~}
	want=${want//-/_}
	[[ $(q '%{VERSION}-%{RELEASE}') == "$want-1" ]] || fail "$name: version $(q '%{VERSION}-%{RELEASE}')"
	[[ $(q '%{BUILDHOST}') == sentinel-watchdog ]] || fail "$name: build host $(q '%{BUILDHOST}')"
	[[ $(q '%{BUILDTIME}') == "$EPOCH" ]] || fail "$name: build time $(q '%{BUILDTIME}'), want $EPOCH"
	q '[%{FILEMODES:perms} %{FILEUSERNAME}:%{FILEGROUPNAME} %{FILENAMES}\n]' \
		| awk '{ if ($1 ~ /^d/) $3 = $3 "/"; print }' >"$work/list"
	compare_list "$name" "$COMMON
$RPM_ONLY" "$work/list"
	# Only the configuration file has flags ("c" config, "n" noreplace);
	# "l" marks the license.
	q '[%{FILEFLAGS:fflags}|%{FILENAMES}\n]' | awk -F'|' '$1 != "" && $1 != "l" {print $1, $2}' >"$work/flags"
	[[ $(cat "$work/flags") == "cn /etc/sentinel/sentinel.yaml" ]] || fail "$name: file flags: $(cat "$work/flags")"
	local tag file
	for pair in PREIN:preinstall.sh POSTIN:rpm/post PREUN:rpm/preun POSTUN:rpm/postun; do
		tag=${pair%%:*} file=${pair#*:}
		rpm_script "$pkg" "$tag" >"$work/script"
		same_script "$name" "$tag" "$SCRIPTS/$file" "$work/script"
		[[ $(q "%{${tag}PROG}") == /bin/sh ]] || fail "$name: $tag interpreter $(q "%{${tag}PROG}")"
	done
	rm -rf "$work/fs" && mkdir "$work/fs"
	(cd "$work/fs" && rpm2cpio "$pkg" | cpio -idm --quiet)
	check_fs "$name" "$work/fs" "$arch" systemd
}

check_apk() { # <file> <arch>
	local pkg=$1 arch=$2 name
	name=$(basename "$pkg")
	rm -rf "$work/apk" && mkdir "$work/apk"
	# An APK is concatenated gzip tar streams: signature (none), control, data.
	tar -xzif "$pkg" -C "$work/apk" --warning=no-unknown-keyword
	tar -tzvif "$pkg" --warning=no-unknown-keyword | awk '$6 !~ /^\./ {p="/" $6; sub(/\/root/, ":root", $2); print $1, $2, p}' >"$work/list"
	compare_list "$name" "$COMMON
$APK_ONLY" "$work/list"
	grep -qx "pkgname = sentinel-watchdog" "$work/apk/.PKGINFO" || fail "$name: package name"
	grep -qx "arch = $arch" "$work/apk/.PKGINFO" || fail "$name: architecture: $(grep '^arch' "$work/apk/.PKGINFO")"
	grep -qx "pkgver = ${VERSION/-/_}" "$work/apk/.PKGINFO" || fail "$name: version: $(grep '^pkgver' "$work/apk/.PKGINFO")"
	same_script "$name" .pre-install "$SCRIPTS/preinstall.sh" "$work/apk/.pre-install"
	same_script "$name" .pre-upgrade "$SCRIPTS/preinstall.sh" "$work/apk/.pre-upgrade"
	same_script "$name" .post-upgrade "$SCRIPTS/apk/post-upgrade" "$work/apk/.post-upgrade"
	same_script "$name" .pre-deinstall "$SCRIPTS/apk/pre-deinstall" "$work/apk/.pre-deinstall"
	local control
	for control in "$work/apk"/.[!.]*; do
		case ${control##*/} in
			.PKGINFO | .pre-install | .pre-upgrade | .post-upgrade | .pre-deinstall) ;;
			*) fail "$name: unexpected control file ${control##*/}" ;;
		esac
	done
	check_fs "$name" "$work/apk" "$arch" openrc
	# .PKGINFO is nFPM's, at the epoch; every other member has the commit's time.
	tar -tzvif "$pkg" --full-time --warning=no-unknown-keyword | grep -v ' \.PKGINFO$' >"$work/raw"
	same_times "$name" "$work/raw"
}

# one <glob>: prints the single file matching it; fails for none or several.
one() {
	# shellcheck disable=SC2206 # $1 is a glob to expand here
	local matches=("$DIST"/$1)
	[[ ${#matches[@]} -eq 1 && -f ${matches[0]} ]] || return 1
	echo "${matches[0]}"
}

main() {
	local a deb rpm f count
	for a in "amd64 x86_64" "arm64 aarch64"; do
		read -r deb rpm <<<"$a"
		if f=$(one "*_${deb}.deb"); then check_deb "$f" "$deb"; else fail "want one *_${deb}.deb"; fi
		if f=$(one "*.${rpm}.rpm"); then check_rpm "$f" "$rpm"; else fail "want one *.${rpm}.rpm"; fi
		if f=$(one "*_${rpm}.apk"); then check_apk "$f" "$rpm"; else fail "want one *_${rpm}.apk"; fi
	done
	count=$(find "$DIST" -maxdepth 1 -type f \( -name '*.deb' -o -name '*.rpm' -o -name '*.apk' \) | wc -l)
	[[ $count -eq 6 ]] || fail "want 6 packages, found $count"
	if [[ $failures -gt 0 ]]; then
		echo "package-verify: $failures failed" >&2
		exit 1
	fi
	echo "package-verify: ok"
}

main "$@"
