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
- **`tracewriter/` package** — a `TraceRecorder` interface with a
  CTFS-producing implementation (`GoWriter`).  Pre-2026-05-08 the package
  also exposed a Rust FFI writer with a `RustFormat` enum
  (`FMT_JSON`/`FMT_BINARY_V0`/`FMT_BINARY`) selectable through a
  `--format` flag; both were removed in the convention compliance pass
  documented in [`AUDIT-CTFS-2026-05.md`](./AUDIT-CTFS-2026-05.md).
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
ct-print --json ./traces/trace.json
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

The `nix develop` shell hook runs `scripts/detect-trace-format.sh` for
historical reasons (it used to wire the Rust FFI library that backed the
removed `--use-rust-writer` flag); the hook is still useful for building
inside a sibling-aware shell but is no longer required for the recorder
to function.

### Nix package builds

```bash
nix build                    # pure-Go wazero (no FFI)
```

## Testing

```bash
just test                  # full upstream wazero test suite (pure Go)
just test-tracewriter-go   # tracewriter tests (pure Go writer only)
just test-tracewriter      # tracewriter tests including Rust FFI writer
just check-all             # lint + all tracewriter tests
just cross-test            # WASM flow integration tests against sibling codetracer
```

### CI

CI workflows run on self-hosted NixOS runners:

- **CI** (`ci.yml`) — lint, pure-Go tracewriter tests, FFI tracewriter tests
  (clones `codetracer-trace-format` via `.github/sibling-pins` to build
  the FFI library), and Nix flake build verification.
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
# codetracer-trace-format-nim):
ct-print --json ./trace-output/trace.json
```

## Project structure

```
cmd/wazero/         CLI entry point (compile, run, version subcommands)
tracewriter/        TraceRecorder interface, GoWriter, RustWriter, FFI header
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
| [codetracer-trace-format](https://github.com/metacraft-labs/codetracer-trace-format) | Trace format crates, including `codetracer_trace_writer_ffi` |
| [trace_record](https://github.com/metacraft-labs/trace_record) | Go library for trace recording (used by GoWriter) |
| [codetracer-python-recorder](https://github.com/metacraft-labs/codetracer-python-recorder) | Python execution recorder |
| [codetracer-native-backend](https://github.com/metacraft-labs/codetracer-native-backend) | Record/replay backend |
| [codetracer-shell-recorders](https://github.com/metacraft-labs/codetracer-shell-recorders) | Bash/Zsh recorders |

## License

This project is licensed under the Apache 2.0 License — see [LICENSE](LICENSE)
for details.

wazero is a registered trademark of Tetrate.io, Inc.
