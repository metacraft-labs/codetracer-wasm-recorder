#!/usr/bin/env bash
# Rebuilds the recorder-golden wasm fixtures from their .rs sources.
#
# Each fixture is a small Rust program compiled to wasm32-wasip1 with
# full DWARF debug info preserved (debug build, NOT release).
#
# The resulting .wasm are NOT checked in — they are build output, and a
# binary committed beside its source is an unverifiable claim that the
# two still correspond.  Run this script before recording any of the
# examples; the .wasm it writes here are gitignored.
#
# The Go test suite does not use these copies.  It compiles its own
# from `cmd/wazero/testdata/recorder-golden/*.rs` as it runs — see
# `cmd/wazero/recorder_golden_fixtures_test.go`, which owns that build
# and fails loudly rather than skipping when the toolchain is missing.
#
# Usage:
#     RUSTC=/path/to/rustc bash build.sh         # explicit rustc
#     bash build.sh                              # picks `rustc` from PATH
#
# Requirements: a rustc with the `wasm32-wasip1` target installed.
# This repo's Nix devshell ships it (shell.nix combines
# `targets.wasm32-wasip1.stable.rust-std` with `stable.rustc`, both
# pinned by flake.lock), so inside `nix develop` / `direnv exec .`
# there is nothing to install.  Outside Nix:
#     rustup target add wasm32-wasip1
#
# The compiler flags (-g, -C debuginfo=2, -C opt-level=0) ensure the
# recorder's DWARF-driven stepping (interpreter.go lines 753-820) sees
# rich source-line information.

set -euo pipefail

cd "$(dirname "$0")"

RUSTC="${RUSTC:-rustc}"

for src in control_flow.rs nested_calls.rs collections.rs panic_path.rs column_aware.rs; do
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
