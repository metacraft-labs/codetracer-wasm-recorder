#!/usr/bin/env bash
# Do the committed boundary-log vectors still describe the producer?
#
# This repo replays recordings it does not make. The pipeline that makes
# them — `ct-instrument`, the `record-web` daemon, headless Chromium —
# lives in `codetracer` and `codetracer-wasm-instrumenter`, so the vectors
# under `cmd/wazero/testdata/boundary-log/` are captured, not produced
# here, and `just test` stays standalone.
#
# What used to be missing was anything that asked whether the capture was
# still true. The sibling's `wasm-parity-corpus/regenerate.sh` simply
# *copied* fresh recordings into this repo's testdata, so the two agreed
# by construction and went on agreeing after the producer moved — the
# staleness had no observer. This script is the observer.
#
# It records the same three demos from the sibling's current tree,
# in the sibling's own development environment (this repo's shell has no
# Node, no wasm32 target and no Chromium), and hands the results to the
# `crossrepo`-tagged Go check, which compares what the recordings *mean*:
# every recovered crossing, its values, and the format witness that
# decides how a value-less import call is classified.
#
# Usage:
#     scripts/verify-vectors-against-producer.sh
#
# Exit codes:
#   0   the committed vectors still describe what the producer emits
#   1   they do not, or the comparison could not be run
#
# There is deliberately no "skip" outcome. If this cannot run, the answer
# is unknown, and an unknown that reports success is how the vectors got
# six weeks out of date in the first place.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
readonly REPO_ROOT

die() {
	echo "verify-vectors: $*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# The sibling that owns the producer.
# ---------------------------------------------------------------------------
CODETRACER_ROOT="${CODETRACER_ROOT:-}"
if [ -z "$CODETRACER_ROOT" ]; then
	for candidate in "$REPO_ROOT/../codetracer" "$REPO_ROOT/../../codetracer"; do
		if [ -x "$candidate/scripts/materialize-recording.sh" ]; then
			CODETRACER_ROOT="$(cd "$candidate" && pwd -P)"
			break
		fi
	done
fi
[ -n "$CODETRACER_ROOT" ] ||
	die "no sibling codetracer checkout with scripts/materialize-recording.sh; set CODETRACER_ROOT"
[ -x "$CODETRACER_ROOT/scripts/materialize-recording.sh" ] ||
	die "$CODETRACER_ROOT/scripts/materialize-recording.sh is missing or not executable"

echo "verify-vectors: producer at $CODETRACER_ROOT"

# ---------------------------------------------------------------------------
# Record, inside the sibling's environment.
#
# `direnv exec` is the cheap path and what a developer's shell already
# has; `nix develop` is the fallback for CI images that do not run direnv.
# Neither is optional: running the pipeline from *this* shell fails on a
# missing `node`, which is a true statement about this repo and a useless
# one about the vectors.
# ---------------------------------------------------------------------------
if command -v direnv >/dev/null 2>&1; then
	run_in_producer_env() { direnv exec "$CODETRACER_ROOT" "$@"; }
elif command -v nix >/dev/null 2>&1; then
	run_in_producer_env() {
		(cd "$CODETRACER_ROOT" && nix develop '.?submodules=1' --command "$@")
	}
else
	die "neither direnv nor nix is available; the producer's toolchain cannot be entered"
fi

materialize() {
	local fixture="$1"
	run_in_producer_env "$CODETRACER_ROOT/scripts/materialize-recording.sh" "$fixture"
}

declare -A produced=()
for fixture in cross-process-three-trace wasm-parity-corpus wasm-nan-payloads; do
	echo "verify-vectors: recording '$fixture' from the producer's current tree"
	dir="$(materialize "$fixture" | tail -n 1)"
	[ -n "$dir" ] && [ -d "$dir" ] ||
		die "recording '$fixture' produced no directory (got '${dir:-}')"
	produced["$fixture"]="$dir"
done

# ---------------------------------------------------------------------------
# Compare meaning, not bytes.
# ---------------------------------------------------------------------------
echo "verify-vectors: comparing the committed vectors against those recordings"
exec env \
	CODETRACER_ROOT="$CODETRACER_ROOT" \
	CT_PRODUCED_RECORDING_CROSS_PROCESS_THREE_TRACE="${produced[cross-process-three-trace]}" \
	CT_PRODUCED_RECORDING_WASM_PARITY_CORPUS="${produced[wasm-parity-corpus]}" \
	CT_PRODUCED_RECORDING_WASM_NAN_PAYLOADS="${produced[wasm-nan-payloads]}" \
	go test -tags crossrepo -count=1 -timeout 30m \
	-run 'TestTheCommitted.*StillDescribesTheProducer' \
	-v ./internal/boundarylog/
