#!/bin/sh
#
# make depcheck: depcheck.sh <module> <public package patterns...>
# A listing or filter that fails, or comes back without what it must hold,
# fails the check.
set -u

module=$1
shift

fail() {
	echo "depcheck FAILED: $1"
	exit 1
}

# One "pkg P" line per non-standard package, one "P imports I" per import.
fmt='{{if not .Standard}}pkg {{.ImportPath}}{{"\n"}}{{$p := .ImportPath}}{{range .Imports}}{{$p}} imports {{.}}{{"\n"}}{{end}}{{end}}'

host=$(go env GOOS) || fail "go env GOOS"
targets=$host
[ "$host" = windows ] || targets="$host windows"

pub=
all=
for goos in $targets; do
	p=$(GOOS=$goos go list -deps -f "$fmt" "$@") ||
		fail "go list of the public tree (GOOS=$goos)"
	c=$(cd cli && GOOS=$goos go list -deps -f "$fmt" ./cmd/waxflow) ||
		fail "go list of cli/cmd/waxflow (GOOS=$goos)"
	printf '%s\n' "$p" | grep -qxF "pkg $module/server" ||
		fail "the public tree's listing (GOOS=$goos) lacks $module/server"
	printf '%s\n' "$c" | grep -qxF "pkg $module/cli/cmd/waxflow" ||
		fail "the cli listing (GOOS=$goos) lacks $module/cli/cmd/waxflow"
	pub="$pub
$p"
	all="$all
$p
$c"
done

# The module path as a regular expression, and "this package or one under it".
mre=$(printf '%s' "$module" | sed 's/\./\\./g') || fail "sed (module pattern)"
[ -n "$mre" ] || fail "the module pattern came back empty"
testutil="$mre/internal/testutil"

# Each filter sets bad from the listing in $1. A grep status of 1 is "no
# match"; anything above it is a failure.
uniq_bad() {
	[ -z "$bad" ] && return
	bad=$(printf '%s\n' "$bad" | sort -u) || fail "sort"
	[ -n "$bad" ] || fail "sort returned nothing"
}

nonmodule() {
	bad=$(printf '%s\n' "$1" | sed -n 's/^pkg //p') || fail "sed (package lines)"
	[ -n "$bad" ] || fail "a listing lost its package lines"
	bad=$(printf '%s\n' "$bad" | grep -Ev "^$mre(/|\$)")
	[ $? -le 1 ] || fail "grep (stdlib-only clause)"
	uniq_bad
}

testimports() {
	bad=$(printf '%s\n' "$1" | grep -E "^[^ ]+ imports $testutil(/|\$)")
	[ $? -le 1 ] || fail "grep (test-support clause)"
	[ -z "$bad" ] && return
	bad=$(printf '%s\n' "$bad" | grep -Ev "^$testutil(/| )")
	[ $? -le 1 ] || fail "grep (test-support importers)"
	uniq_bad
}

# Each filter must flag a known violation before its clean answer counts.
nonmodule "pkg $module/server
pkg example.invalid/x"
[ "$bad" = example.invalid/x ] || fail "the stdlib-only filter missed a known violation"
for known in "$module/server imports $module/internal/testutil" \
	"$module/server imports $module/internal/testutil/hlsclient"; do
	testimports "$known"
	[ "$bad" = "$known" ] || fail "the test-support filter missed: $known"
done

nonmodule "$pub"
if [ -n "$bad" ]; then
	echo "$bad"
	fail "non-stdlib imports in the public tree (above)"
fi
echo "depcheck ok: public tree ($*) is stdlib-only"

testimports "$all"
if [ -n "$bad" ]; then
	echo "$bad"
	fail "product code imports test support (above)"
fi
echo "depcheck ok: nothing the public tree or cli/cmd/waxflow links imports internal/testutil"
