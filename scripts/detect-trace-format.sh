#!/usr/bin/env bash
# =============================================================================
# Detect the codetracer-trace-format-nim sibling repo and configure CGO
# environment variables so `go test` and `go build` link the wazero binary
# against the Nim C FFI library (`libcodetracer_trace_writer.a`) that drives
# the CTFS writer in `tracewriter/ctfs_writer.go`.
#
# Usage:
#   source scripts/detect-trace-format.sh
#
# Environment (set after sourcing):
#   CODETRACER_TRACE_FORMAT_NIM_PATH — absolute path to the Nim trace-format repo
#   FFI_LIB_DIR                     — directory containing libcodetracer_trace_writer.a
#   FFI_INCLUDE_DIR                 — directory containing codetracer_trace_writer.h
#   CGO_ENABLED                     — 1 when the FFI library is available
#   CGO_CFLAGS                      — -I<FFI_INCLUDE_DIR>
#   CGO_LDFLAGS                     — -L<FFI_LIB_DIR>
#   LD_LIBRARY_PATH                 — updated to include FFI_LIB_DIR (Linux)
#   DYLD_LIBRARY_PATH               — updated to include FFI_LIB_DIR (macOS)
#
# History — 2026-05-08:
#   Pre-Follow-up A this script wired up the Rust `codetracer-trace-format`
#   FFI library (`libcodetracer_trace_writer_ffi.a`).  The wasm recorder has
#   since migrated to the Nim FFI in `codetracer-trace-format-nim` so the
#   `.ct` (CTFS) container is produced directly via cgo — see
#   AUDIT-CTFS-2026-05.md.
# =============================================================================

_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"

# ---------------------------------------------------------------------------
# Locate the sibling codetracer-trace-format-nim repo.
# ---------------------------------------------------------------------------
_TRACE_FORMAT_NIM_DIR=""

# Standard layout: metacraft/codetracer-wasm-recorder/../codetracer-trace-format-nim
_candidate="$(cd "$_REPO_ROOT/.." 2>/dev/null && pwd)/codetracer-trace-format-nim"
if [ -d "$_candidate/include" ] && [ -d "$_candidate/src" ]; then
  _TRACE_FORMAT_NIM_DIR="$_candidate"
fi

# Worktree layout: metacraft/ws/codetracer-wasm-recorder/../../codetracer-trace-format-nim
if [ -z "$_TRACE_FORMAT_NIM_DIR" ]; then
  _candidate="$(cd "$_REPO_ROOT/../.." 2>/dev/null && pwd)/codetracer-trace-format-nim"
  if [ -d "$_candidate/include" ] && [ -d "$_candidate/src" ]; then
    _TRACE_FORMAT_NIM_DIR="$_candidate"
  fi
fi

if [ -z "$_TRACE_FORMAT_NIM_DIR" ]; then
  echo "  detect-trace-format: codetracer-trace-format-nim sibling not found." >&2
  echo "  CTFS writer requires the Nim FFI; the wazero binary will fail to link." >&2
  unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate
  return 0 2>/dev/null || exit 0
fi

export CODETRACER_TRACE_FORMAT_NIM_PATH="$_TRACE_FORMAT_NIM_DIR"

# ---------------------------------------------------------------------------
# Build the FFI library if not already built.
#
# The Nim repo's nimble file exposes a `buildLib` task that produces the
# static library + header next to the source tree.  We only invoke it when
# the .a file is missing so repeat shell-entries are fast.
# ---------------------------------------------------------------------------
_FFI_LIB="$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.a"
_FFI_INCLUDE_DIR="$_TRACE_FORMAT_NIM_DIR/include"

if [ ! -f "$_FFI_LIB" ]; then
  echo "  detect-trace-format: building libcodetracer_trace_writer.a (first time)..." >&2
  if command -v nimble >/dev/null 2>&1; then
    (cd "$_TRACE_FORMAT_NIM_DIR" && nimble buildLib) || {
      echo "  detect-trace-format: ERROR: nimble buildLib failed." >&2
      unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR
      return 1 2>/dev/null || exit 1
    }
  else
    echo "  detect-trace-format: ERROR: nimble not found; cannot build FFI." >&2
    unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR
    return 1 2>/dev/null || exit 1
  fi
  echo "  detect-trace-format: FFI library built successfully." >&2
else
  echo "  detect-trace-format: Nim FFI library found at $_FFI_LIB" >&2
fi

export FFI_LIB_DIR="$_TRACE_FORMAT_NIM_DIR"
export FFI_INCLUDE_DIR="$_FFI_INCLUDE_DIR"
export CGO_ENABLED=1
export CGO_CFLAGS="-I${_FFI_INCLUDE_DIR}"
export CGO_LDFLAGS="-L${_TRACE_FORMAT_NIM_DIR}"

# ---------------------------------------------------------------------------
# Locate libzstd — the Nim FFI links against zstd for CTFS chunk
# compression.  Inside the Nix dev shell, libzstd is not pulled in by
# default, so we resolve it via nix-build and fold the path into the cgo
# linker / runtime environment.  Outside Nix (CI on bare distros), the
# system loader will find libzstd via /etc/ld.so.conf and the loop is a
# no-op.
# ---------------------------------------------------------------------------
if command -v nix-build >/dev/null 2>&1; then
  _ZSTD_OUT="$(nix-build --no-out-link '<nixpkgs>' -A zstd.out 2>/dev/null || true)"
  if [ -n "$_ZSTD_OUT" ] && [ -d "$_ZSTD_OUT/lib" ]; then
    export CGO_LDFLAGS="${CGO_LDFLAGS} -L${_ZSTD_OUT}/lib -Wl,-rpath,${_ZSTD_OUT}/lib"
    case "$(uname -s)" in
      Linux*)  export LD_LIBRARY_PATH="${_ZSTD_OUT}/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
      Darwin*) export DYLD_LIBRARY_PATH="${_ZSTD_OUT}/lib${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" ;;
    esac
    echo "  detect-trace-format: linking against libzstd at $_ZSTD_OUT" >&2
  fi
  unset _ZSTD_OUT
fi

# Platform-specific dynamic library path for the Nim FFI itself (only
# relevant if the consumer switches to the .so build via
# `nimble buildSharedLib`; harmless otherwise).
case "$(uname -s)" in
  Linux*)  export LD_LIBRARY_PATH="${_TRACE_FORMAT_NIM_DIR}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
  Darwin*) export DYLD_LIBRARY_PATH="${_TRACE_FORMAT_NIM_DIR}${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" ;;
esac

echo "  detect-trace-format: CGO_ENABLED=1, FFI_LIB_DIR=$_TRACE_FORMAT_NIM_DIR" >&2

# ---------------------------------------------------------------------------
# Clean up temporary variables.
# ---------------------------------------------------------------------------
unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR
