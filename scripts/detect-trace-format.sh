#!/usr/bin/env bash
# =============================================================================
# Detect and build the codetracer_trace_writer_ffi library from the sibling
# codetracer-trace-format repo. Exports CGO environment variables so that
# `go test` and `go build` with CGO_ENABLED=1 can link against the FFI
# library.
#
# Usage:
#   source scripts/detect-trace-format.sh
#
# Environment (set after sourcing):
#   CODETRACER_TRACE_FORMAT_PATH — absolute path to the trace-format repo
#   FFI_LIB_DIR                  — directory containing the built .a/.so/.dylib
#   CGO_ENABLED                  — set to 1 when the FFI library is available
#   CGO_LDFLAGS                  — linker flags for the FFI library
#   LD_LIBRARY_PATH              — updated to include FFI_LIB_DIR (Linux)
#   DYLD_LIBRARY_PATH            — updated to include FFI_LIB_DIR (macOS)
#
# If the sibling repo is not found, all variables remain unset and a warning
# is printed. Pure-Go tests will still work (CGO_ENABLED stays at 0).
# =============================================================================

_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"

# ---------------------------------------------------------------------------
# Locate the sibling codetracer-trace-format repo.
# ---------------------------------------------------------------------------
_TRACE_FORMAT_DIR=""

# Standard layout: metacraft/codetracer-wasm-recorder/../codetracer-trace-format
_candidate="$(cd "$_REPO_ROOT/.." 2>/dev/null && pwd)/codetracer-trace-format"
if [ -d "$_candidate/codetracer_trace_writer_ffi" ]; then
  _TRACE_FORMAT_DIR="$_candidate"
fi

# Worktree layout: metacraft/ws/codetracer-wasm-recorder/../../codetracer-trace-format
if [ -z "$_TRACE_FORMAT_DIR" ]; then
  _candidate="$(cd "$_REPO_ROOT/../.." 2>/dev/null && pwd)/codetracer-trace-format"
  if [ -d "$_candidate/codetracer_trace_writer_ffi" ]; then
    _TRACE_FORMAT_DIR="$_candidate"
  fi
fi

if [ -z "$_TRACE_FORMAT_DIR" ]; then
  echo "  detect-trace-format: codetracer-trace-format sibling not found." >&2
  echo "  Rust FFI writer will not be available. Pure-Go writer still works." >&2
  unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_DIR _candidate
  return 0 2>/dev/null || exit 0
fi

export CODETRACER_TRACE_FORMAT_PATH="$_TRACE_FORMAT_DIR"

# ---------------------------------------------------------------------------
# Build the FFI library if not already built.
# ---------------------------------------------------------------------------
_FFI_LIB_DIR="$_TRACE_FORMAT_DIR/target/release"
_FFI_STATIC="$_FFI_LIB_DIR/libcodetracer_trace_writer_ffi.a"

if [ ! -f "$_FFI_STATIC" ]; then
  echo "  detect-trace-format: building codetracer_trace_writer_ffi (first time)..." >&2
  (cd "$_TRACE_FORMAT_DIR" && cargo build -p codetracer_trace_writer_ffi --release) || {
    echo "  detect-trace-format: ERROR: failed to build FFI library." >&2
    unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_DIR _candidate _FFI_LIB_DIR _FFI_STATIC
    return 1 2>/dev/null || exit 1
  }
  echo "  detect-trace-format: FFI library built successfully." >&2
else
  echo "  detect-trace-format: FFI library found at $_FFI_LIB_DIR" >&2
fi

export FFI_LIB_DIR="$_FFI_LIB_DIR"
export CGO_ENABLED=1
export CGO_LDFLAGS="-L${_FFI_LIB_DIR}"

# Platform-specific dynamic library path.
case "$(uname -s)" in
  Linux*)  export LD_LIBRARY_PATH="${_FFI_LIB_DIR}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
  Darwin*) export DYLD_LIBRARY_PATH="${_FFI_LIB_DIR}${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" ;;
esac

echo "  detect-trace-format: CGO_ENABLED=1, FFI_LIB_DIR=$_FFI_LIB_DIR" >&2

# ---------------------------------------------------------------------------
# Clean up temporary variables.
# ---------------------------------------------------------------------------
unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_DIR _candidate _FFI_LIB_DIR _FFI_STATIC
