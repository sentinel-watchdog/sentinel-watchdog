#!/bin/sh
# Table test of packaging/pkg-version.sh (D-102): git describe output in,
# package version out; then the package manager of the image says each
# version is valid (apk) and that the order is right (dpkg, apk).
#
# Usage: tests/packaging/version_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed.
set -eu
readonly VERSION_SH="$1/packaging/pkg-version.sh"
failures=0

# expect <git describe output> <want version, or "error">
expect() {
	got=$(sh "$VERSION_SH" "$1" 2>/dev/null) || got=error
	if [ "$got" = "$2" ]; then
		echo "ok   '$1' -> $2"
	else
		echo "FAIL '$1' -> $got, want $2" >&2
		failures=$((failures + 1))
	fi
}

expect "v0.3.0-0-g1e43ede" 0.3.0
expect "v0.3.0-0-g1e43ede-dirty" 0.3.1-pre0
expect "v0.3.0-5-g1e43ede" 0.3.1-pre5
expect "v0.3.0-12-g1e43ede-dirty" 0.3.1-pre12
expect "v1.9.9-1-gabcdef0" 1.9.10-pre1
expect "" 0.0.1-pre0
expect "v0.3" error
expect "v0.3.0-rc.1-0-g1e43ede" error
expect "0.3.0-0-g1e43ede" error
expect "v0.3.0-x-g1e43ede" error

# The forms above as each package manager writes them (nFPM: "-" becomes
# "~" for deb, "_" for apk).
if command -v apk >/dev/null 2>&1; then
	for v in 0.3.0 0.3.1_pre0 0.3.1_pre5 1.9.10_pre1 0.0.1_pre0; do
		apk version -c "$v" >/dev/null || {
			echo "FAIL apk rejects $v" >&2
			failures=$((failures + 1))
		}
	done
	for pair in "0.3.0 0.3.1_pre0" "0.3.1_pre0 0.3.1_pre5" "0.3.1_pre5 0.3.1_pre12" "0.3.1_pre12 0.3.1"; do
		# shellcheck disable=SC2086 # split the pair into two arguments
		set -- $pair
		[ "$(apk version -t "$1" "$2")" = "<" ] || {
			echo "FAIL apk: $1 not before $2" >&2
			failures=$((failures + 1))
		}
	done
fi
if command -v dpkg >/dev/null 2>&1; then
	for pair in "0.3.0 0.3.1~pre0" "0.3.1~pre0 0.3.1~pre5" "0.3.1~pre5 0.3.1~pre12" "0.3.1~pre12 0.3.1"; do
		# shellcheck disable=SC2086 # split the pair into two arguments
		set -- $pair
		dpkg --compare-versions "$1" lt "$2" || {
			echo "FAIL dpkg: $1 not before $2" >&2
			failures=$((failures + 1))
		}
	done
fi

[ "$failures" -eq 0 ] && echo "version_test: ok"
[ "$failures" -eq 0 ]
