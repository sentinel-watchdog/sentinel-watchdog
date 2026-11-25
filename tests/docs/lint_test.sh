#!/bin/sh
# Test of tests/docs/lint.go on small trees: one finding per broken link,
# anchor or dash, none for what GitHub renders.
#
# Usage: tests/docs/lint_test.sh <repo dir>
# Exit status: 0 all cases pass, 1 a case failed.
set -eu
root=$(cd "$1" && pwd)
work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT
go build -o "$work/lint" "$root/tests/docs/lint.go"
failures=0

# expect <name> [<file:line>...]: lints the files of $work/tree, which must
# give exactly these findings, in this order, then removes them.
expect() {
	name=$1
	shift
	want=$(for at in "$@"; do echo "$at"; done)
	want_status=0
	[ $# -eq 0 ] || want_status=1
	status=0
	(cd "$work/tree" && find . -type f | sed 's,^\./,,' | tr '\n' '\0' | "$work/lint") >"$work/out" 2>&1 || status=$?
	got=$(sed -n 's/^\([^ ]*:[0-9]*\): .*/\1/p' "$work/out")
	if [ "$got" = "$want" ] && [ "$status" -eq "$want_status" ]; then
		echo "ok   $name"
	else
		echo "FAIL $name: exit $status, want $want_status; findings: $got" >&2
		cat "$work/out" >&2
		failures=$((failures + 1))
	fi
	rm -rf "$work/tree"
	mkdir "$work/tree"
}
mkdir "$work/tree"
cd "$work/tree"
put() { (cd "$work/tree" && mkdir -p "$(dirname "$1")" && cat >"$1"); }

put a.md <<'X'
# Title, with `code` and: marks
## Same
## Same
## Perché no?
<a id="here"></a>
[ok](#title-with-code-and-marks) [two](#same-1) [id](#here) [file](d/b.md) [dir](d) [dir](d/)
[anchor](d/b.md#sub_part) [line](d/x.go#L3) [root](/d/b.md) [space](d/a%20b.txt) [utf8](#perché-no)
[web](https://example.com/none#x) [mail](mailto:a@b.c)
![image](d/i.txt "title") [ref][r] `[code](none.md)`
```text
[fenced](none.md)
```
[r]: d/b.md
X
put d/b.md <<'X'
# Sub_part
[up](../a.md#same) [root](/a.md#same)
X
put d/x.go </dev/null
put d/i.txt </dev/null
put "d/a b.txt" </dev/null
printf 'go test fuzz v1\nstring("a \342\200\224 b")\n' | put p/testdata/fuzz/FuzzX/0a
expect "what resolves"

put a.md <<'X'
[a](none.md)
[b](#none)
[c](b.md#none)
[r]: gone.md
![i](gone.png)
[out](../outside.md)
<img src="gone.svg">
X
put b.md <<'X'
# B
X
expect "what does not resolve" a.md:1 a.md:2 a.md:3 a.md:4 a.md:5 a.md:6 a.md:7

printf 'one \342\200\224 two\n' | put a.txt
printf 'one \342\200\223 two\n' | put b.go
expect "dashes" a.txt:1 b.go:1

printf 'x\000\342\200\224\n' | put a.bin
expect "binary files"

printf '# A\n~~~\n```\n[x](none.md)\n~~~\n[y](none.md)\n' | put a.md
expect "a fence ends with its own marker" a.md:6

# shellcheck disable=SC2016 # literal Markdown, not an expansion
printf '```text\n```yaml\nx\n```\n[x](none.md)\n' | put a.md
expect "a closing fence has no text" a.md:5

# shellcheck disable=SC2016 # literal Markdown, not an expansion
printf '`` ` `` then [x](none.md) and `ok`\n' | put a.md
expect "a code span ends with as many backticks" a.md:1

# shellcheck disable=SC2016 # literal Markdown, not an expansion
printf 'a `f[T](x,\ny)` b\n\none ` tick\n\n[z](none.md) `\n' | put a.md
expect "code spans cross lines, not paragraphs" a.md:6

# shellcheck disable=SC2016 # literal Markdown, not an expansion
printf '%s\n' '\` then ``` then [x](none.md) then \`' | put a.md
expect "escaped backticks start no code span" a.md:1

if printf '' | "$work/lint" >/dev/null 2>&1; then
	echo "FAIL no input is accepted" >&2
	failures=$((failures + 1))
else
	echo "ok   no input is an error"
fi

[ "$failures" -eq 0 ] && echo "docs lint_test: ok"
[ "$failures" -eq 0 ]
