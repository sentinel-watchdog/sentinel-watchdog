#!/bin/sh
# Test of tests/fuzz/run.sh with a stub go: a failing target, or a package
# that does not list its targets, must not hide the targets after it.
#
# Usage: tests/fuzz/run_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed.
set -eu
root=$(cd "$1" && pwd)
work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT
mkdir "$work/bin"
cat >"$work/bin/go" <<'STUB'
#!/bin/sh
echo "$*" >>"$CALLS"
case "$*" in
	"list ./...") printf 'p/a\np/broken\np/b\n' ;;
	*"-list ^Fuzz p/a") printf 'FuzzOne\nFuzzTwo\nok  \tp/a\t0.1s\n' ;;
	*"-list ^Fuzz p/broken") exit "$BROKEN" ;;
	*"-list ^Fuzz p/b") printf 'FuzzThree\nok  \tp/b\t0.1s\n' ;;
	*"-fuzz ^$FAIL\$ "*) exit 1 ;;
esac
STUB
chmod +x "$work/bin/go"
failures=0

# run <failing target> <status of listing p/broken>: sets status and ran.
run() {
	: >"$work/calls"
	status=0
	PATH="$work/bin:$PATH" CALLS="$work/calls" FAIL="$1" BROKEN="$2" \
		sh "$root/tests/fuzz/run.sh" 7s >"$work/out" 2>&1 || status=$?
	ran=$(grep -c -- '-fuzztime 7s' "$work/calls" || true)
}

# expect <what> <got> <want>
expect() {
	if [ "$2" = "$3" ]; then
		echo "ok   $1"
	else
		echo "FAIL $1: got $2, want $3" >&2
		failures=$((failures + 1))
	fi
}

run none 0
expect "all pass: status" "$status" 0
expect "all pass: targets run" "$ran" 3
run FuzzOne 0
expect "first fails: status" "$status" 1
expect "first fails: targets run" "$ran" 3
expect "first fails: named" "$(grep -c '^fuzz: failed: p/a FuzzOne$' "$work/out")" 1
run FuzzThree 0
expect "last fails: status" "$status" 1
expect "last fails: targets run" "$ran" 3
run none 1
expect "package does not build: status" "$status" 1
expect "package does not build: targets run" "$ran" 3
expect "package does not build: named" "$(grep -c '^fuzz: failed: p/broken (targets not listed)$' "$work/out")" 1

[ "$failures" -eq 0 ] && echo "fuzz run_test: ok"
[ "$failures" -eq 0 ]
