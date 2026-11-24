#!/usr/bin/env bash
# Test of the steps of release.yml that decide what is signed and published
# (D-103). Those jobs check out nothing, so the steps stay inline in the
# workflow; this test extracts each one by job and name and runs it as the
# runner does, on made-up files with stub cosign and gh. The real signing is
# proved only by a run on GitHub.
#
# Usage: tests/packaging/release_workflow_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed or a step was not found.
set -euo pipefail
workflow="$(cd "$1" && pwd)/.github/workflows/release.yml"
readonly workflow
failures=0
work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT

# step <job> <step name> <file>: the step's script, without its indentation.
step() {
	awk -v job="  $1:" -v name="      - name: $2" '
		/^  [a-z]+:$/ { injob = ($0 == job) }
		injob && /^      - / { instep = ($0 == name); inrun = 0 }
		inrun && /^          / { print substr($0, 11); next }
		inrun && /^$/ { print; next }
		{ inrun = 0 }
		instep && /^        run: \|$/ { inrun = 1 }
	' "$workflow" >"$3"
	[ -s "$3" ] || {
		echo "release_workflow_test: no step '$2' in job $1" >&2
		exit 1
	}
}
step sign "Check that the files are exactly those in checksums.txt" "$work/sign.sh"
step sign "Sign checksums.txt" "$work/cosign.sh"
step publish "Verify the signature, then the files" "$work/publish.sh"
step publish "Create the draft release" "$work/draft.sh"

mkdir "$work/stub"
cat >"$work/stub/gh" <<'STUB'
#!/bin/sh
case $1 in
	api)
		[ -z "${API_FAIL:-}" ] || exit 1
		printf '%s\n' "${RELEASES:-}"
		;;
	*) echo "$*" >"$RUNNER_TEMP/gh.log" ;;
esac
STUB
chmod 0755 "$work/stub/gh"

# fresh <"" or bundle>: a new directory for one case, with its files in
# $assets; with "bundle", the signature too.
fresh() {
	run=$(mktemp -d "$work/run.XXXXXX")
	assets=$run/assets
	mkdir "$assets" "$run/temp"
	cat >"$run/temp/cosign" <<'STUB'
#!/bin/sh
echo "$*" >>"$RUNNER_TEMP/cosign.log"
exit "${COSIGN_STATUS:-0}"
STUB
	chmod 0755 "$run/temp/cosign"
	(
		cd "$assets"
		echo a >a.deb
		echo b >b.rpm
		sha256sum a.deb b.rpm >checksums.txt
	)
	[ "$1" != bundle ] || echo '{}' >"$assets/checksums.txt.sigstore.json"
}

# expect <ok|error> <case> <script> [VAR=value ...]
expect() {
	local want=$1 name=$2 script=$3 got=ok
	shift 3
	(
		cd "$run"
		env PATH="$work/stub:$PATH" RUNNER_TEMP="$run/temp" \
			GITHUB_REPOSITORY=o/r GITHUB_REF=refs/tags/v0.3.1 \
			GITHUB_SERVER_URL=https://github.com TAG=v0.3.1 "$@" \
			bash --noprofile --norc -eo pipefail "$work/$script"
	) >"$work/out" 2>&1 || got=error
	if [ "$got" = "$want" ]; then
		echo "ok   $script: $name -> $want"
	else
		echo "FAIL $script: $name -> $got, want $want" >&2
		cat "$work/out" >&2
		failures=$((failures + 1))
	fi
}

for script in sign.sh publish.sh; do
	with=
	[ "$script" = sign.sh ] || with=bundle
	fresh "$with"
	expect ok "the listed files" "$script"
	fresh "$with"
	echo x >"$assets/extra.deb"
	expect error "a file that is not listed" "$script"
	fresh "$with"
	echo x >"$assets/.hidden"
	expect error "a hidden file" "$script"
	fresh "$with"
	mkdir "$assets/dir"
	echo x >"$assets/dir/checksums.txt"
	expect error "a directory" "$script"
	fresh "$with"
	mv "$assets/a.deb" "$run/temp/"
	expect error "a missing file" "$script"
	fresh "$with"
	echo x >>"$assets/a.deb"
	expect error "a changed file" "$script"
	fresh "$with"
	: >"$assets/checksums.txt"
	expect error "an empty list" "$script"
	fresh "$with"
	mv "$assets/checksums.txt" "$run/temp/"
	expect error "no list" "$script"
	fresh "$with"
	cat "$assets/checksums.txt" "$assets/checksums.txt" >"$run/temp/twice"
	mv "$run/temp/twice" "$assets/checksums.txt"
	expect error "files listed twice" "$script"
done
fresh bundle
expect error "the signature among the files to sign" sign.sh
fresh ""
expect error "no signature" publish.sh
fresh bundle
expect error "cosign refuses the signature" publish.sh COSIGN_STATUS=1

# identity <script>: cosign verified for exactly this repository, workflow
# and tag (a pattern or another tag would accept other signatures).
identity() {
	grep -qE "^verify-blob .*--certificate-identity https://github.com/o/r/.github/workflows/release.yml@refs/tags/v0.3.1 --certificate-oidc-issuer https://token.actions.githubusercontent.com (assets/)?checksums.txt\$" "$run/temp/cosign.log" || {
		echo "FAIL $1: cosign verify-blob without the exact identity" >&2
		failures=$((failures + 1))
	}
}
fresh ""
expect ok "signs and verifies" cosign.sh
identity cosign.sh
fresh bundle
expect ok "verifies again" publish.sh
identity publish.sh
fresh ""
expect error "cosign fails" cosign.sh COSIGN_STATUS=1

# created <text>...: the recorded gh call has every text.
created() {
	local text
	for text in "$@"; do
		grep -qF -e "$text" "$run/temp/gh.log" || {
			echo "FAIL draft.sh: gh call without '$text'" >&2
			failures=$((failures + 1))
		}
	done
}
fresh bundle
expect ok "a v0 tag" draft.sh
created "release create v0.3.1 --repo o/r --draft --verify-tag" --prerelease \
	assets/a.deb assets/b.rpm assets/checksums.txt assets/checksums.txt.sigstore.json
fresh bundle
expect ok "a v1 tag" draft.sh TAG=v1.0.0
created "release create v1.0.0 --repo o/r --draft --verify-tag"
if grep -q -e --prerelease "$run/temp/gh.log"; then
	echo "FAIL draft.sh: v1.0.0 is a prerelease" >&2
	failures=$((failures + 1))
fi
fresh bundle
expect ok "another release exists" draft.sh RELEASES=v0.3.10
fresh bundle
expect error "the release exists" draft.sh RELEASES=v0.3.1
fresh bundle
expect error "the releases cannot be listed" draft.sh API_FAIL=1

[ "$failures" -eq 0 ] && echo "release_workflow_test: ok"
[ "$failures" -eq 0 ]
