#!/usr/bin/env bash
# Verify that the codetracer-wasm-recorder CLI (the upstream `wazero` binary
# extended with CodeTracer tracing) complies with
# `Recorder-CLI-Conventions.md` (no silent skip — every assertion either
# passes or fails loudly):
#
#   * `--format` is absent from `--help` and `run --help` (CTFS-only —
#     convention §4)
#   * `CODETRACER_FORMAT` is absent from `--help` (convention §5)
#   * `--out-dir`, `--help`, `-h` are present in `run --help` (§3)
#   * `--help` mentions `ct print` (the canonical conversion tool, §4)
#   * `CODETRACER_WASM_RECORDER_OUT_DIR` /
#     `CODETRACER_WASM_RECORDER_DISABLED` are referenced in source so the
#     env-var fallback (§5) cannot regress silently.
#
# Special: §1 documents the wazero binary as the one exception to the
# `codetracer-<lang>-recorder` naming convention — this script intentionally
# does not assert on the binary name (it is `wazero`, not
# `codetracer-wasm-recorder`).
#
# Wire-up: see `Justfile` (`just verify-cli-convention` runs this script;
# `just check-all` chains it after the test suite).
#
# Exit codes:
#   0  all assertions held
#   1  at least one assertion failed (the failing line is printed to
#      stderr and the script exits at the first failure for clarity)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Build the binary if it isn't already built.
( cd "${REPO_ROOT}" && go build -o "${REPO_ROOT}/wazero" ./cmd/wazero )

BIN="${REPO_ROOT}/wazero"
if [[ ! -x "${BIN}" ]]; then
  echo "ERROR: wazero binary not found at ${BIN}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

assert_absent() {
  # assert_absent <needle> <haystack-description> <haystack>
  local needle="$1"
  local desc="$2"
  local haystack="$3"
  if grep -qF -- "${needle}" <<< "${haystack}"; then
    echo "FAIL: ${desc} must NOT contain '${needle}'" >&2
    echo "----- ${desc} -----" >&2
    echo "${haystack}" >&2
    echo "-------------------" >&2
    exit 1
  fi
  echo "ok: '${needle}' absent from ${desc}"
}

assert_present() {
  # assert_present <needle> <haystack-description> <haystack>
  local needle="$1"
  local desc="$2"
  local haystack="$3"
  if ! grep -qF -- "${needle}" <<< "${haystack}"; then
    echo "FAIL: ${desc} must contain '${needle}'" >&2
    echo "----- ${desc} -----" >&2
    echo "${haystack}" >&2
    echo "-------------------" >&2
    exit 1
  fi
  echo "ok: '${needle}' present in ${desc}"
}

# ---------------------------------------------------------------------------
# Top-level --help
# ---------------------------------------------------------------------------
#
# wazero prints help to stderr and exits 0 (upstream behaviour).  We
# capture both streams together so the assertions are stable regardless of
# any future Go-side reshuffle.
TOP_HELP="$( "${BIN}" -h 2>&1 || true )"

assert_absent "--format" "top-level --help" "${TOP_HELP}"
assert_absent "CODETRACER_FORMAT" "top-level --help" "${TOP_HELP}"
assert_present "compile" "top-level --help" "${TOP_HELP}"
assert_present "run" "top-level --help" "${TOP_HELP}"
assert_present "version" "top-level --help" "${TOP_HELP}"
assert_present "ct print" "top-level --help" "${TOP_HELP}"
assert_present "CODETRACER_WASM_RECORDER_OUT_DIR" "top-level --help" "${TOP_HELP}"
assert_present "CODETRACER_WASM_RECORDER_DISABLED" "top-level --help" "${TOP_HELP}"

# ---------------------------------------------------------------------------
# `run` subcommand --help
# ---------------------------------------------------------------------------

RUN_HELP="$( "${BIN}" run -h 2>&1 || true )"

assert_absent "--format" "run --help" "${RUN_HELP}"
assert_absent "CODETRACER_FORMAT" "run --help" "${RUN_HELP}"
assert_present "-out-dir" "run --help" "${RUN_HELP}"
assert_present "ct print" "run --help" "${RUN_HELP}"
assert_present "CODETRACER_WASM_RECORDER_OUT_DIR" "run --help" "${RUN_HELP}"
assert_present "CODETRACER_WASM_RECORDER_DISABLED" "run --help" "${RUN_HELP}"

# ---------------------------------------------------------------------------
# `compile` subcommand --help (sanity — must not advertise --format either)
# ---------------------------------------------------------------------------

COMPILE_HELP="$( "${BIN}" compile -h 2>&1 || true )"

assert_absent "--format" "compile --help" "${COMPILE_HELP}"
assert_absent "CODETRACER_FORMAT" "compile --help" "${COMPILE_HELP}"

# ---------------------------------------------------------------------------
# --version output (use the `version` subcommand which is the canonical
# wazero pattern)
# ---------------------------------------------------------------------------

VERSION_OUT="$( "${BIN}" version 2>&1 )"
# wazero prints either "dev" or a semver string; non-empty is sufficient.
if [[ -z "${VERSION_OUT}" ]]; then
  echo "FAIL: version output must be non-empty" >&2
  exit 1
fi
echo "ok: version output non-empty (${VERSION_OUT})"

# ---------------------------------------------------------------------------
# Source-level reference for the env-var fallback
# ---------------------------------------------------------------------------

# The recorder must reference CODETRACER_WASM_RECORDER_OUT_DIR /
# CODETRACER_WASM_RECORDER_DISABLED in source (otherwise the env-var
# fallback either doesn't exist or has been silently removed).  We grep
# under cmd/.
if ! grep -rqF "CODETRACER_WASM_RECORDER_OUT_DIR" "${REPO_ROOT}/cmd"; then
  echo "FAIL: CODETRACER_WASM_RECORDER_OUT_DIR must be referenced in cmd/" >&2
  exit 1
fi
echo "ok: CODETRACER_WASM_RECORDER_OUT_DIR referenced in cmd/"

if ! grep -rqF "CODETRACER_WASM_RECORDER_DISABLED" "${REPO_ROOT}/cmd"; then
  echo "FAIL: CODETRACER_WASM_RECORDER_DISABLED must be referenced in cmd/" >&2
  exit 1
fi
echo "ok: CODETRACER_WASM_RECORDER_DISABLED referenced in cmd/"

# ---------------------------------------------------------------------------
# The Rust FFI writer should no longer be present (post-2026-05-08
# convention compliance).  Catch a partial revert that leaves the FFI
# files behind.
# ---------------------------------------------------------------------------

if [[ -f "${REPO_ROOT}/tracewriter/rust_writer.go" ]]; then
  echo "FAIL: tracewriter/rust_writer.go must not exist (CTFS-only — see AUDIT-CTFS-2026-05.md)" >&2
  exit 1
fi
echo "ok: tracewriter/rust_writer.go absent"

echo "verify-cli-convention-no-silent-skip: all checks passed"
