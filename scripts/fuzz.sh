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
# instantly with "--- FAIL"; either is a real finding. A compile/list error is
# surfaced too, rather than being silently skipped into a false green.
#
# New Fuzz* functions are picked up automatically.
#
# Usage: scripts/fuzz.sh [fuzztime]      (default 20m per target)
# Env:   FUZZ_LOG_DIR   directory for per-target logs (default ./fuzz-logs)

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

fuzztime="${1:-20m}"
log_dir="${FUZZ_LOG_DIR:-fuzz-logs}"
mkdir -p "$log_dir"

pkgs=$(go list ./...) || {
	err "go list ./... failed"
	exit 1
}

status=0

for pkg in $pkgs; do
	# Compile the test binary and list its fuzz targets. A compile/list error
	# must fail the job, not be swallowed into a false green.
	if ! listing=$(go test -list '^Fuzz' "$pkg" 2>&1); then
		err "cannot compile/list fuzz targets in $pkg"
		printf '%s\n' "$listing" >&2
		status=1
		continue
	fi

	# Same target name lives in several packages (FuzzDecode, FuzzDemux), so
	# qualify the log by package or a later run clobbers an earlier one.
	pkg_safe=$(printf '%s' "$pkg" | tr '/' '_')

	for target in $(printf '%s\n' "$listing" | grep '^Fuzz' || true); do
		log="$log_dir/${pkg_safe}_${target}.log"

		group "fuzz $target ($fuzztime) — $pkg"
		# tee so progress still streams to the console while we keep the full
		# output for classification; PIPESTATUS[0] is go test's real exit code.
		go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" -v "$pkg" 2>&1 | tee "$log"
		rc=${PIPESTATUS[0]}
		endgroup

		if [ "$rc" -eq 0 ]; then
			echo "ok   $target — completed $fuzztime with no findings"
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
			warn "$target hit the -fuzztime shutdown artifact (no reproducer) — not a finding, treating as pass"
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
done

if [ "$status" -ne 0 ]; then
	echo
	echo "fuzz: real findings above. Reproduce and grow the regression corpus with:"
	echo "  go test -run='^\$' -fuzz='^FuzzName\$' ./path/to/pkg   # then commit testdata/fuzz/…"
fi
exit "$status"
