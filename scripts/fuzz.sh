#!/usr/bin/env bash
#
# Run every native Fuzz* target for a fixed time budget and classify the
# outcome, so CI turns red only on a real finding.
#
# Go's fuzzing engine exits non-zero with "context deadline exceeded" when
# -fuzztime elapses while a worker is still mid-execution: no crashing input
# was found and nothing is written to testdata/fuzz. That end-of-run artifact
# (golang/go #49046, #56238, #70431, #72104) must not be mistaken for a bug.
# A genuine crasher instead writes a reproducer under testdata/fuzz and prints
# "Failing input written to ..."; a broken committed seed/corpus entry fails
# instantly with "--- FAIL"; either is a real finding. A package whose tests
# do not build fails when its target runs, and a name the build turns out not
# to have is an error here rather than the PASS Go gives it.
#
# New Fuzz* functions are picked up automatically, by name: go list reports
# each package's test files under the current build constraints and a target
# is a top-level `func Fuzz*` in one of them, the same list `go test -list`
# gives without linking every test binary in the tree first.
#
# Usage: scripts/fuzz.sh [fuzztime]      (default 20m per target)
# Env:   FUZZTIME       per-target budget when no argument is given. The
#                       Makefile passes it as the argument, so honoring it here
#                       too means `FUZZTIME=60s scripts/fuzz.sh` does what it
#                       reads like rather than silently taking the 20m default.
#        FUZZ_SHARD     k/N runs every Nth target of the discovered list,
#                       starting at the kth (1-based), so N jobs given 1/N
#                       through N/N cover the list exactly once between them.
#                       CI uses it: the whole list is hours of fuzzing at the
#                       nightly budget, past what one hosted job may run.
#        FUZZ_LOG_DIR   directory for per-target logs (default ./fuzz-logs)
#        FUZZ_PARALLEL  workers per target; unset lets Go use one per core.
#
# On worker count and memory: the engine's footprint grows with run length and
# scales with worker count, in steps rather than smoothly. Measured on a
# 24-core box, one target over five minutes reached 5.1 GB across its workers
# with the largest at 1.9 GB, and it is the engine rather than any one target
# (the heaviest reading came from the oldest target in the tree). Targets run
# one at a time here, so that is the whole footprint, and it is fine on an
# ordinary machine. FUZZ_PARALLEL is the lever if a box has many cores and
# little memory; CI runners have few enough cores that the default already
# bounds them.

set -u

# GitHub Actions log annotations; plain lines when run locally.
if [ -n "${GITHUB_ACTIONS:-}" ]; then
	group() { echo "::group::$*"; }
	endgroup() { echo "::endgroup::"; }
	err() { echo "::error::$*"; }
	warn() { echo "::warning::$*"; }
else
	group() { echo "==> $*"; }
	endgroup() { :; }
	err() { echo "ERROR: $*" >&2; }
	warn() { echo "warning: $*" >&2; }
fi

# Every discovery command below is relative to the module root.
if [ ! -f go.mod ]; then
	err "run scripts/fuzz.sh from the repository root (no go.mod in $(pwd))"
	exit 1
fi

fuzztime="${1:-${FUZZTIME:-20m}}"
# Validated before any compile, so a typo fails in a second, not an hour.
shard_index=1
shard_count=1
if [ -n "${FUZZ_SHARD:-}" ]; then
	if [[ "$FUZZ_SHARD" =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] && [ "${BASH_REMATCH[1]}" -le "${BASH_REMATCH[2]}" ]; then
		shard_index=${BASH_REMATCH[1]}
		shard_count=${BASH_REMATCH[2]}
	else
		err "FUZZ_SHARD must be k/N with 1 <= k <= N, got '$FUZZ_SHARD'"
		exit 1
	fi
fi

log_dir="${FUZZ_LOG_DIR:-fuzz-logs}"
mkdir -p "$log_dir"

# One line per test file, tab-separated so a checkout path with a space
# survives: directory, import path, file.
listing=$(go list -f '{{range .TestGoFiles}}{{$.Dir}}{{"\t"}}{{$.ImportPath}}{{"\t"}}{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.Dir}}{{"\t"}}{{$.ImportPath}}{{"\t"}}{{.}}{{"\n"}}{{end}}' ./...) || {
	err "go list ./... failed"
	exit 1
}

status=0

# Unquoted on use, so an unset FUZZ_PARALLEL expands to nothing and Go keeps
# its own default rather than being handed an empty flag.
parallel=""
if [ -n "${FUZZ_PARALLEL:-}" ]; then
	parallel="-parallel $FUZZ_PARALLEL"
fi

# Discover every target before running any, so the shard arithmetic sees the
# whole list in one fixed order (go list's, then file, then line). gofmt pins
# the spelling the pattern relies on.
targets=()
while IFS=$'\t' read -r dir pkg file; do
	[ -n "$file" ] || continue
	for target in $(grep -hoE '^func Fuzz[A-Za-z0-9_]*' "$dir/$file" | cut -c6- || true); do
		targets+=("$pkg $target")
	done
done <<<"$listing"

if [ "${#targets[@]}" -eq 0 ]; then
	err "no Fuzz* targets found under $(pwd)"
	exit 1
fi

selected=()
i=0
for entry in "${targets[@]}"; do
	if [ $((i % shard_count)) -eq $((shard_index - 1)) ]; then
		selected+=("$entry")
	fi
	i=$((i + 1))
done

if [ "${#selected[@]}" -eq 0 ]; then
	err "shard $shard_index/$shard_count selects none of the ${#targets[@]} targets: more shards than targets"
	exit 1
fi

if [ "$shard_count" -gt 1 ]; then
	echo "fuzz: shard $shard_index/$shard_count runs ${#selected[@]} of ${#targets[@]} targets at $fuzztime each"
	for entry in "${selected[@]}"; do
		echo "  ${entry#* }  (${entry% *})"
	done
fi

for entry in "${selected[@]}"; do
	pkg=${entry% *}
	target=${entry#* }

	# Same target name lives in several packages (FuzzDecode, FuzzDemux), so
	# qualify the log by package or a later run clobbers an earlier one.
	pkg_safe=$(printf '%s' "$pkg" | tr '/' '_')
	log="$log_dir/${pkg_safe}_${target}.log"

	group "fuzz $target ($fuzztime) - $pkg"
	# tee so progress still streams to the console while we keep the full
	# output for classification; PIPESTATUS[0] is go test's real exit code.
	# shellcheck disable=SC2086
	go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" $parallel -v "$pkg" 2>&1 | tee "$log"
	rc=${PIPESTATUS[0]}
	endgroup

	# Go answers a name the build does not have with PASS and exit 0 (and a
	# warning only when the package has other targets), so a pass has to
	# show the target ran.
	if [ "$rc" -eq 0 ] && ! grep -q "^=== RUN   ${target}\$" "$log"; then
		err "$target is not a fuzz target of $pkg as built"
		status=1
		continue
	fi

	if [ "$rc" -eq 0 ]; then
		echo "ok   $target - completed $fuzztime with no findings"
		continue
	fi

	# A generated crasher (t.Fatal/panic on a mutated input) always writes
	# a reproducer. Echo the report and the bytes so the finding is visible
	# without downloading the artifact.
	if grep -q "Failing input written to" "$log"; then
		err "CRASHER: $target found a new reproducing input"
		sed -n '/^--- FAIL/,$p' "$log"
		pkgdir=$(go list -f '{{.Dir}}' "$pkg")
		if [ -d "$pkgdir/testdata/fuzz/$target" ]; then
			find "$pkgdir/testdata/fuzz/$target" -type f -print | while read -r f; do
				echo "--- reproducer: $f ---"
				cat "$f"
			done
		fi
		status=1
		continue
	fi

	# The benign -fuzztime artifact also prints "--- FAIL", so it MUST be
	# classified before the generic seed/corpus check below.
	if grep -q "context deadline exceeded" "$log"; then
		warn "$target hit the -fuzztime shutdown artifact (no reproducer) - not a finding, treating as pass"
		continue
	fi

	# A committed testdata/fuzz entry or an f.Add seed failed: a regression,
	# even though no new reproducer was written.
	if grep -q '^--- FAIL' "$log"; then
		err "REGRESSION: $target failed on a seed/corpus input"
		sed -n '/^--- FAIL/,$p' "$log"
		status=1
		continue
	fi

	# Anything else: a build/vet/infra error, not a fuzz outcome.
	err "$target failed to run (exit $rc)"
	tail -n 40 "$log"
	status=1
done

if [ "$status" -ne 0 ]; then
	echo
	echo "fuzz: real findings above. Reproduce and grow the regression corpus with:"
	echo "  go test -run='^\$' -fuzz='^FuzzName\$' ./path/to/pkg   # then commit testdata/fuzz/…"
fi
exit "$status"
