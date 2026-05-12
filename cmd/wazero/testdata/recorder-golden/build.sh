#!/usr/bin/env bash
# Rebuilds the recorder-golden wasm fixtures from their .rs sources.
#
# Each fixture is a small Rust program compiled to wasm32-wasip1 with
# full DWARF debug info preserved (debug build, NOT release).  The
# resulting .wasm files are checked into the repo per the
# wasm-tracing branch convention so the Go tests under
# `cmd/wazero/recorder_golden_test.go` don't need a working Rust
# toolchain to run.  This script exists so a developer can rebuild
# them when the .rs sources change.
#
# Usage:
#     RUSTC=/path/to/rustc bash build.sh         # explicit rustc
#     bash build.sh                              # picks `rustc` from PATH
#
# Requirements: a rustc with the `wasm32-wasip1` target installed.
# The Nix devshell does NOT ship this target by default; install it
# via rustup:
#     rustup target add wasm32-wasip1
#
# The compiler flags (-g, -C debuginfo=2, -C opt-level=0) ensure the
# recorder's DWARF-driven stepping (interpreter.go lines 753-820) sees
# rich source-line information.

set -euo pipefail

cd "$(dirname "$0")"

RUSTC="${RUSTC:-rustc}"

for src in control_flow.rs nested_calls.rs collections.rs panic_path.rs; do
    out="${src%.rs}.wasm"
    echo "building $src -> $out"
    "$RUSTC" \
        -g \
        -C debuginfo=2 \
        -C opt-level=0 \
        --edition 2021 \
        --target wasm32-wasip1 \
        -o "$out" \
        "$src"
done

echo "done."
