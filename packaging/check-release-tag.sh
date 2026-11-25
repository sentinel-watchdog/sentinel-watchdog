#!/bin/sh
# Refuse to build a release unless the checkout is exactly the tag (D-103),
# for Taskfile.yml's release-build. The tag must be vX.Y.Z (no leading
# zeros, no suffix: apk cannot order prerelease tags, D-102), and both
# versions Task derived from git must be that tag: a dirty tree, commits
# after the tag or a second tag on the commit would put another version in
# the binaries or in the packages. git describe does not see untracked
# files, which a build would include, so the tree must have none (nor an
# ignored one where the build reads its sources); and the
# commit must be on origin/main. Run in the checkout.
# Usage: check-release-tag.sh <tag> <binary version> <package version>.
# Exit 1 on any mismatch.
set -eu

tag=${1:-}
binary=${2:-}
package=${3:-}

fail() {
	echo "check-release-tag: $*" >&2
	exit 1
}

case $tag in v*) ;; *) fail "'$tag' is not vX.Y.Z" ;; esac
version=${tag#v}
# Digits and dots only, so the split below sees no newline or glob character.
case $version in
	*[!0-9.]* | .* | *. | *..*) fail "'$tag' is not vX.Y.Z" ;;
esac
count=0
IFS=.
for part in $version; do
	count=$((count + 1))
	case $part in 0?*) fail "'$tag' has a number with a leading zero" ;; esac
done
[ "$count" -eq 3 ] || fail "'$tag' is not vX.Y.Z"

# git describe prefers an annotated tag and hides the others (B-054).
tags=$(git tag --points-at HEAD) || fail "git tag failed"
[ "$tags" = "$tag" ] || fail "the tags on this commit are not just '$tag': $(printf '%s' "${tags:-none}" | tr '\n' ' ')"
[ "$binary" = "$tag" ] || fail "the binaries would say '$binary', not '$tag'"
[ "$package" = "$version" ] || fail "the packages would be '$package', not '$version'"

changes=$(git status --porcelain --untracked-files=all) || fail "git status failed"
ignored=$(git status --porcelain --ignored --untracked-files=all -- cmd internal pkg vendor go.work packaging deploy configs) || fail "git status failed"
[ -z "$changes$ignored" ] || fail "the checkout has changed, untracked or ignored files: git status --ignored"
git merge-base --is-ancestor HEAD refs/remotes/origin/main || fail "the commit is not on origin/main"
