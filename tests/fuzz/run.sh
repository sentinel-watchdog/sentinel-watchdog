#!/bin/sh
# Run every Fuzz* target of the module for a time each, all of them even
# when one fails (task fuzz, .github/workflows/fuzz.yml). Run in the module.
#
# Usage: tests/fuzz/run.sh <fuzztime>
# Exit status: 0 no target failed, 1 a target or a package failed.
set -eu
fuzztime=$1
status=0
failed() {
	echo "fuzz: failed: $*" >&2
	status=1
}

# Assignments, not `for x in $(cmd)`: a failing go command is seen.
pkgs=$(go list ./...)
for pkg in $pkgs; do
	listed=$(go test -list '^Fuzz' "$pkg") || {
		failed "$pkg (targets not listed)"
		continue
	}
	for target in $(printf '%s\n' "$listed" | grep '^Fuzz' || true); do
		go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg" || failed "$pkg $target"
	done
done
exit "$status"
