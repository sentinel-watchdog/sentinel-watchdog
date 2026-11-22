#!/bin/sh
# Package version from git describe output (D-102), for Taskfile.yml's
# PKG_VERSION. The input is `git describe --long --dirty` against release
# tags only (vX.Y.Z): prerelease tags are excluded, since apk can order only
# a few fixed suffixes.
#
#   v0.3.0-0-g<hash>        -> 0.3.0        (on the tag, clean)
#   v0.3.0-0-g<hash>-dirty  -> 0.3.1-pre0   (a dirty tree is not the release)
#   v0.3.0-5-g<hash>        -> 0.3.1-pre5   (5 commits after the tag)
#   (empty: no tag)         -> 0.0.1-pre0
#
# "-preN" becomes ~preN for deb and rpm and _preN for apk (nFPM): each sorts
# after the tag and before the next release.
# Usage: pkg-version.sh <git describe output>. Exit 1 for any other input.
set -eu

d=${1:-}
[ -n "$d" ] || {
	echo 0.0.1-pre0
	exit 0
}
dirty=
case $d in *-dirty)
	dirty=1
	d=${d%-dirty}
	;;
esac
rest=${d%-g*}
count=${rest##*-}
tag=${rest%-*}
version=${tag#v}
case $tag in v*) ;; *) version=. ;; esac
case $version in
	*[!0-9.]* | .* | *. | *..*) version=. ;;
esac
case $count in '' | *[!0-9]*) version=. ;; esac
# Exactly three numbers.
[ "$(echo "$version" | tr -cd . | wc -c | tr -d ' ')" = 2 ] || {
	echo "pkg-version: not a release tag description: '$1'" >&2
	exit 1
}
if [ "$count" = 0 ] && [ -z "$dirty" ]; then
	echo "$version"
	exit 0
fi
patch=${version##*.}
echo "${version%.*}.$((patch + 1))-pre${count}"
