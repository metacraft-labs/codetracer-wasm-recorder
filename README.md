# CodeTracer WASM Recorder

A fork of [wazero](https://github.com/tetratelabs/wazero) — the zero-dependency
WebAssembly runtime for Go — with [CodeTracer](https://github.com/metacraft-labs/codetracer)
execution tracing built in.

## How this differs from upstream wazero

Upstream wazero is a conformant WebAssembly runtime that compiles and executes
`.wasm` modules with zero external dependencies. This fork adds **execution
tracing**: as a WASM program runs, every step, function call, return, and
variable mutation is recorded into a CodeTracer trace that can be loaded into
the CodeTracer time-traveling debugger.

Key additions on top of upstream wazero:

- **`--out-dir` flag** on the `run` subcommand — produces a CTFS trace bundle
  that the CodeTracer time-travel debugger can load.  Falls back to the
  `CODETRACER_WASM_RECORDER_OUT_DIR` environment variable when omitted.
- **`CODETRACER_WASM_RECORDER_DISABLED` environment variable** — when set,
  runs the target through wazero without writing any trace artefacts.
  Useful for wrapping wazero under a recording-aware CI / IDE harness
  without rebuilding the command line.
- **`--stylus` flag** — enables EVM hook functions for Arbitrum Stylus
  `debug_traceTransaction` support.
- **`tracewriter/` package** — a `TraceRecorder` interface and a single
  CTFS-producing implementation (`CtfsTraceWriter`) that links via cgo
  into the Nim C FFI exposed by
  [`codetracer-trace-format-nim`](https://github.com/metacraft-labs/codetracer-trace-format-nim).
  Pre-2026-05-08 the package also exposed a pure-Go writer (`GoWriter`,
  legacy three-file JSON layout) and a Rust FFI writer with a
  `RustFormat` enum (`FMT_JSON`/`FMT_BINARY_V0`/`FMT_BINARY`)
  selectable through a `--format` flag; all were removed in the
  convention compliance pass documented in
  [`AUDIT-CTFS-2026-05.md`](./AUDIT-CTFS-2026-05.md).
- **Tracing hooks** in `internal/` — wazero internals are instrumented to
  call the `TraceRecorder` at each execution step.

### Binary name

The CodeTracer recorder convention names binaries
`codetracer-<language>-recorder` (see
[`codetracer-specs/Recorder-CLI-Conventions.md`](../codetracer-specs/Recorder-CLI-Conventions.md)
§1).  The wasm recorder is the **one documented exception** — this is a
fork of Tetrate's [wazero](https://github.com/tetratelabs/wazero) with
tracing layered in, not a CodeTracer-named tool, so the binary keeps the
upstream `wazero` name.  Convention §1 explicitly carves this out.

The upstream wazero code is kept largely intact; tracing is injected via
hooks rather than invasive modifications, making it straightforward to
merge upstream updates.

### Output format

The recorder always writes CTFS (Recorder-CLI-Conventions.md §4).  There is
no `--format` flag and no `CODETRACER_FORMAT` environment variable.  For
human-readable inspection of a recorded trace, pipe the bundle through
`ct print` from
[`codetracer-trace-format-nim`](https://github.com/metacraft-labs/codetracer-trace-format-nim):

```bash
wazero run --out-dir ./traces program.wasm
ct-print --full --strip-paths ./traces/program.ct
```

## Building

### Prerequisites

- **Go 1.22+** (tested with Go 1.24)
- **Nix** (recommended) — provides all dependencies via `nix develop`

### Quick start (Nix)

```bash
nix develop          # enters dev shell with Go, Rust, and tools
just build           # produces ./wazero binary
```

### Quick start (without Nix)

```bash
go build -o wazero ./cmd/wazero    # pure-Go build (no FFI)
```

### Workspace layout

The recorder lives alongside its sibling repos in the metacraft workspace:

```
metacraft/
├── codetracer-wasm-recorder/    # this repo
├── codetracer-trace-format-nim/ # ships the `ct-print` CLI used by tests
├── codetracer/                  # main CodeTracer repo
└── ...
```

The `nix develop` shell hook runs `scripts/detect-trace-format.sh` to
locate the sibling `codetracer-trace-format-nim` checkout, build the Nim
C FFI static library if necessary, and export the cgo flags needed to
link the wazero binary against `libcodetracer_trace_writer.a` +
`libzstd`.  The recorder will fail to link without these inputs.

### Nix package builds

```bash
nix build                    # wazero with the CTFS-stub fallback (recording non-functional)
```

To produce a recording-capable binary, the consuming flake must pass a
pre-built `codetracer-trace-format-nim` derivation to `wazero.nix`.

## Testing

```bash
just test                  # full upstream wazero test suite + cgo CTFS writer
just test-tracewriter-go   # tracewriter package tests (currently empty)
just check-all             # lint + tracewriter tests + CLI convention check
just cross-test            # WASM flow integration tests against sibling codetracer
```

### CI

CI workflows run on self-hosted NixOS runners:

- **CI** (`ci.yml`) — lint, tracewriter tests, and Nix flake build
  verification.  The CTFS writer test path requires the
  `codetracer-trace-format-nim` sibling (cloned via
  `.github/sibling-pins`) so the cgo build can link against
  `libcodetracer_trace_writer.a`.
- **Cross-Repo Integration Tests** (`cross-repo-tests.yml`) — builds the
  wazero binary and runs end-to-end WASM flow tests in the codetracer repo's
  db-backend. Supports bidirectional triggering via `repository_dispatch` so
  the codetracer repo can trigger these tests when WASM-related code changes.
  Uses the standard sibling-pins priority cascade (workflow_dispatch >
  repository_dispatch > sibling-pins > fallback).
- **Upstream Tests** (`commit.yaml`) — the original wazero test matrix
  (multi-platform, multi-Go-version, scratch container, BSD VMs, fuzzing).

## Usage

```bash
# Run a WASM program and produce a CTFS trace bundle:
./wazero run --out-dir ./trace-output program.wasm

# Same, but configured through the env-var fallback (Recorder-CLI-Conventions.md §5):
CODETRACER_WASM_RECORDER_OUT_DIR=./trace-output ./wazero run program.wasm

# Run the target without recording — useful when a wrapping harness
# always invokes wazero with --out-dir but the user wants to skip
# recording for one invocation:
CODETRACER_WASM_RECORDER_DISABLED=1 ./wazero run --out-dir ./trace-output program.wasm

# Mount a directory and pass environment variables:
./wazero run --mount ./data:/data --env KEY=VALUE program.wasm

# Arbitrum Stylus debug tracing:
./wazero run --out-dir ./trace-output --stylus ./evm-hooks.so program.wasm

# Convert a recorded bundle to JSON via ct print (sibling repo
# codetracer-trace-format-nim).  The output is a single
# `<program-basename>.ct` (CTFS) container; ct-print --full decodes it
# back to JSON.
ct-print --full --strip-paths ./trace-output/program.ct
```

## Project structure

```
cmd/wazero/         CLI entry point (compile, run, version subcommands)
tracewriter/        TraceRecorder interface + cgo CtfsTraceWriter + Nim FFI header
scripts/            Dev environment and cross-repo test scripts
internal/           Upstream wazero internals with tracing hooks
experimental/       Upstream experimental features (logging, sockets)
vendor/             Vendored Go dependencies
```

## Sibling repos

This repo is part of the [CodeTracer](https://github.com/metacraft-labs/codetracer)
ecosystem. Related repositories:

| Repository | Purpose |
|:-----------|:--------|
| [codetracer](https://github.com/metacraft-labs/codetracer) | Main CodeTracer debugger (Nim + Electron) |
| [codetracer-trace-format-nim](https://github.com/metacraft-labs/codetracer-trace-format-nim) | Nim CTFS writer + C FFI consumed by the cgo CtfsTraceWriter |
| [trace_record](https://github.com/metacraft-labs/trace_record) | Go library exporting the value-record types used in `tracewriter/` |
| [codetracer-python-recorder](https://github.com/metacraft-labs/codetracer-python-recorder) | Python execution recorder |
| [codetracer-native-backend](https://github.com/metacraft-labs/codetracer-native-backend) | Record/replay backend |
| [codetracer-shell-recorders](https://github.com/metacraft-labs/codetracer-shell-recorders) | Bash/Zsh recorders |

## License

This project is licensed under the Apache 2.0 License — see [LICENSE](LICENSE)
for details.

wazero is a registered trademark of Tetrate.io, Inc.
