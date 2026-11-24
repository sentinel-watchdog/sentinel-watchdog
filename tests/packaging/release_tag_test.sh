#!/bin/sh
# Table test of packaging/check-release-tag.sh (D-103): which tags the
# release workflow may build, given the versions Task derives from git, in
# throwaway repositories. Needs git; no root and no container.
#
# Usage: tests/packaging/release_tag_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed.
set -eu
root=$(cd "$1" && pwd)
readonly CHECK_SH="$root/packaging/check-release-tag.sh"
readonly VERSION_SH="$root/packaging/pkg-version.sh"
failures=0
work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT
# Nothing from the caller's git configuration or repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

# A clean repository whose only commit is on origin/main.
repo() {
	git init -q "$work/$1"
	cd "$work/$1"
	echo one >file
	git add file
	git commit -qm one
	git update-ref refs/remotes/origin/main HEAD
}
repo table

# expect <ok|error> <tag> <binary version> <package version>
expect() {
	status=0
	sh "$CHECK_SH" "$2" "$3" "$4" >/dev/null 2>&1 || status=$?
	case $status in
		0) got=ok ;;
		1) got=error ;;
		*) got="exit $status" ;;
	esac
	if [ "$got" = "$1" ]; then
		echo "ok   '$2' '$3' '$4' -> $1"
	else
		echo "FAIL '$2' '$3' '$4' -> $got, want $1" >&2
		failures=$((failures + 1))
	fi
}

expect ok v0.3.1 v0.3.1 0.3.1
expect ok v1.0.0 v1.0.0 1.0.0
expect ok v10.20.30 v10.20.30 10.20.30
expect ok v0.0.0 v0.0.0 0.0.0

# Not vX.Y.Z: the package managers could not order it, or it is not a version.
expect error "" "" ""
expect error 0.3.1 0.3.1 0.3.1
expect error v0.3 v0.3 0.3
expect error v0.3.1.4 v0.3.1.4 0.3.1.4
expect error v0..1 v0..1 0..1
expect error v.3.1 v.3.1 .3.1
expect error v0.3. v0.3. 0.3.
expect error v1.0.0-rc.1 v1.0.0-rc.1 1.0.0-rc.1
expect error v1.0.0+b1 v1.0.0+b1 1.0.0+b1
expect error v01.0.0 v01.0.0 01.0.0
expect error v1.00.0 v1.00.0 1.00.0
expect error v1.0.01 v1.0.01 1.0.01
# Text a shell or a file name would read as something else.
# shellcheck disable=SC2016 # literal text, not an expansion
expect error 'v1.0.0$(id)' 'v1.0.0$(id)' '1.0.0$(id)'
expect error 'v1.0.0;id' 'v1.0.0;id' '1.0.0;id'
expect error 'v1.0.0/../x' 'v1.0.0/../x' '1.0.0/../x'
expect error 'v1.0.0 ' 'v1.0.0 ' '1.0.0 '
expect error 'v1.0.*' 'v1.0.*' '1.0.*'
expect error 'v1.0.0
v1.0.0' 'v1.0.0
v1.0.0' '1.0.0
1.0.0'

# The checkout is not the tag: changed files, commits after the tag, or
# another tag on the same commit.
expect error v0.3.1 v0.3.1-dirty 0.3.2-pre0
expect error v0.3.1 v0.3.1-2-gabcdef0 0.3.2-pre2
expect error v0.3.1 v0.3.2 0.3.2
expect error v0.3.1 nightly 0.3.1
expect error v0.3.1 v0.3.1 0.3.2
expect error V0.3.1 V0.3.1 0.3.1
expect error v0.3.1 "" ""

# The same with the versions as Taskfile.yml derives them (VERSION and
# PKG_VERSION), in repositories in each state.
# expect_repo <ok|error> <tag>
expect_repo() {
	binary=$(git describe --tags --always --dirty)
	package=$(sh "$VERSION_SH" "$(git describe --tags --match 'v[0-9]*' --exclude 'v*[!0-9.]*' --long --dirty 2>/dev/null || true)")
	expect "$1" "$2" "$binary" "$package"
}
repo clean
git tag v0.3.1
expect_repo ok v0.3.1
repo untracked
git tag v0.3.1
echo x >extra.go
expect_repo error v0.3.1
repo ignored
git tag v0.3.1
mkdir cmd
echo x >cmd/extra.go
echo '*.go' >>.git/info/exclude
expect_repo error v0.3.1
repo ignored-output
git tag v0.3.1
mkdir dist
echo x >dist/x
echo dist >>.git/info/exclude
expect_repo ok v0.3.1
repo modified
git tag v0.3.1
echo two >file
expect_repo error v0.3.1
repo later
git tag v0.3.1
git commit -q --allow-empty -m two
git update-ref refs/remotes/origin/main HEAD
expect_repo error v0.3.1
repo off-main
git commit -q --allow-empty -m two
git tag v0.3.1
expect_repo error v0.3.1
repo no-main
git tag v0.3.1
git update-ref -d refs/remotes/origin/main
expect_repo error v0.3.1

[ "$failures" -eq 0 ] && echo "release_tag_test: ok"
[ "$failures" -eq 0 ]
