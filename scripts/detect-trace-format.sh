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
# Build the FFI library if it is missing or older than its sources.
#
# The Nim repo's nimble file exposes a `buildStaticLib` task that produces
# the static library + header next to the source tree.  We invoke it when the
# library is missing, or when any of the sibling's sources is newer than it:
# a checkout that moves the sibling to another revision touches the files it
# changes, and a library built from the previous revision would otherwise keep
# writing the previous format.  Repeat shell entries stay fast.  (The task was
# called `buildLib` at one point; `codetracer_trace_format.nimble` now spells
# the static and shared halves apart as `buildStaticLib` / `buildSharedLib`,
# and invoking the old name fails with "task not found".)
# ---------------------------------------------------------------------------
_FFI_INCLUDE_DIR="$_TRACE_FORMAT_NIM_DIR/include"
# The FFI artifact may be either the static library (`.a`, produced by
# `nimble buildStaticLib`) or the shared library (`.so`, produced by the workspace's
# `build-siblings.sh` via the sibling repo's own dev shell).  cgo links
# `-lcodetracer_trace_writer`, which the toolchain resolves from EITHER form in
# the `-L` dir, so accept whichever is present.
if [ -f "$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.a" ]; then
  _FFI_LIB="$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.a"
elif [ -f "$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.so" ]; then
  _FFI_LIB="$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.so"
else
  _FFI_LIB=""
fi

# A library is stale when a source it is built from (`src/`, the `.nimble`)
# is newer than it.
if [ -n "$_FFI_LIB" ] && [ -n "$(find "$_TRACE_FORMAT_NIM_DIR/src" "$_TRACE_FORMAT_NIM_DIR"/*.nimble \
  -newer "$_FFI_LIB" -type f \( -name '*.nim' -o -name '*.c' -o -name '*.h' -o -name '*.nimble' \) \
  -print 2>/dev/null | head -n 1)" ]; then
  echo "  detect-trace-format: $_FFI_LIB is older than its sources; rebuilding." >&2
  _FFI_LIB=""
fi

if [ -z "$_FFI_LIB" ]; then
  echo "  detect-trace-format: building the trace-writer FFI..." >&2
  # The FFI build needs nim/nimble + zstd, which live in the SIBLING repo's own
  # dev shell, NOT this wasm recorder's shell.  Build the dependency in the
  # sibling's dev shell instead of polluting this shell with those tools:
  #   * `repro exec <sibling>` if the sibling declares an environment;
  #   * else `nix develop <sibling>` (the sibling ships a flake).
  # Explicitly enter the sibling before invoking either wrapper, and `nimble` resolves its project from the cwd
  # only (it does not search upwards).  Without the `cd` they run in whatever
  # directory the shell was entered from — the recorder repo — and fail with
  # "Could not find a file with a .nimble extension".  The subshell keeps the
  # `cd` from leaking into the shell that sourced this script.
  _BUILD_OK=""
  if command -v repro >/dev/null 2>&1; then
    (cd "$_TRACE_FORMAT_NIM_DIR" && repro exec "$_TRACE_FORMAT_NIM_DIR" -- nimble -d:release buildStaticLib) && _BUILD_OK=1
  fi
  if [ -z "$_BUILD_OK" ] && command -v nix >/dev/null 2>&1; then
    (cd "$_TRACE_FORMAT_NIM_DIR" && nix develop "$_TRACE_FORMAT_NIM_DIR" --command nimble -d:release buildStaticLib) && _BUILD_OK=1
  fi
  if [ -z "$_BUILD_OK" ] && command -v nimble >/dev/null 2>&1; then
    # Last resort: nimble is on PATH in this shell after all.
    (cd "$_TRACE_FORMAT_NIM_DIR" && nimble buildStaticLib) && _BUILD_OK=1
  fi
  if [ -z "$_BUILD_OK" ]; then
    echo "  detect-trace-format: ERROR: could not build the FFI in the sibling dev shell." >&2
    echo "  Build it once with: (cd $_TRACE_FORMAT_NIM_DIR && nix develop --command nimble buildStaticLib)" >&2
    unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR _BUILD_OK
    return 1 2>/dev/null || exit 1
  fi
  # `buildStaticLib` writes the `.a`; anything else would be a stale `.so`.
  _FFI_LIB="$_TRACE_FORMAT_NIM_DIR/libcodetracer_trace_writer.a"
  unset _BUILD_OK
  echo "  detect-trace-format: FFI library built successfully ($_FFI_LIB)." >&2
else
  echo "  detect-trace-format: Nim FFI library found at $_FFI_LIB" >&2
fi
# When linking the shared library, it must also be discoverable at run time.
case "$(uname -s)" in
  Linux*)  export LD_LIBRARY_PATH="${_TRACE_FORMAT_NIM_DIR}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
  Darwin*) export DYLD_LIBRARY_PATH="${_TRACE_FORMAT_NIM_DIR}${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" ;;
esac

# ---------------------------------------------------------------------------
# Link exactly the library chosen above.
#
# cgo links `-lcodetracer_trace_writer`.  Pointed at the sibling directory, the
# linker takes the `.so` whenever one sits next to the `.a`, whatever its age,
# so the library chosen above would not be the one linked.  And Go's build
# cache keys a link on CGO_LDFLAGS, not on the content of the libraries it
# names, so a rebuilt library under an unchanged flag is not relinked.  The
# `-L` directory is therefore one that holds only the chosen library, named by
# a checksum of its content: a different library is a different flag.
# ---------------------------------------------------------------------------
_FFI_SUM="$(cksum <"$_FFI_LIB" | cut -d' ' -f1)"
_FFI_LINK_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/codetracer-wasm-recorder/ffi-link/${_FFI_SUM}-$(basename "$_FFI_LIB")"
mkdir -p "$_FFI_LINK_DIR"
ln -sfn "$_FFI_LIB" "$_FFI_LINK_DIR/$(basename "$_FFI_LIB")"

export FFI_LIB_DIR="$_TRACE_FORMAT_NIM_DIR"
export FFI_INCLUDE_DIR="$_FFI_INCLUDE_DIR"
export CGO_ENABLED=1
export CGO_CFLAGS="-I${_FFI_INCLUDE_DIR}"
export CGO_LDFLAGS="-L${_FFI_LINK_DIR}"

# ---------------------------------------------------------------------------
# Locate libzstd — the Nim FFI links against zstd for CTFS chunk
# compression (`-lzstd` in tracewriter/ctfs_writer.go).  The Nix dev shell
# names the library from the flake's pinned nixpkgs in
# CODETRACER_WASM_ZSTD_LIB_DIR (shell.nix), and it is folded into the cgo
# linker / runtime environment here.  Outside Nix (bare distros) the variable
# is unset and the system linker finds libzstd in its default search path.
#
# This used to ask `nix-build '<nixpkgs>' -A zstd.out` instead, with errors
# discarded.  That resolves whatever nixpkgs the host's NIX_PATH names — not
# the flake's pin — and on a host with no channel it silently resolved
# nothing, so the link failed with "cannot find -lzstd".
# ---------------------------------------------------------------------------
if [ -n "${CODETRACER_WASM_ZSTD_LIB_DIR:-}" ]; then
  if [ ! -d "$CODETRACER_WASM_ZSTD_LIB_DIR" ]; then
    echo "  detect-trace-format: ERROR: CODETRACER_WASM_ZSTD_LIB_DIR=$CODETRACER_WASM_ZSTD_LIB_DIR is not a directory." >&2
    unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR _FFI_SUM _FFI_LINK_DIR
    return 1 2>/dev/null || exit 1
  fi
  export CGO_LDFLAGS="${CGO_LDFLAGS} -L${CODETRACER_WASM_ZSTD_LIB_DIR} -Wl,-rpath,${CODETRACER_WASM_ZSTD_LIB_DIR}"
  case "$(uname -s)" in
    Linux*)  export LD_LIBRARY_PATH="${CODETRACER_WASM_ZSTD_LIB_DIR}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
    Darwin*) export DYLD_LIBRARY_PATH="${CODETRACER_WASM_ZSTD_LIB_DIR}${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" ;;
  esac
  echo "  detect-trace-format: linking against libzstd in $CODETRACER_WASM_ZSTD_LIB_DIR" >&2
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
unset _SCRIPT_DIR _REPO_ROOT _TRACE_FORMAT_NIM_DIR _candidate _FFI_LIB _FFI_INCLUDE_DIR _FFI_SUM _FFI_LINK_DIR
