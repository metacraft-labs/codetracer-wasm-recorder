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

- **`--out-dir` flag** on the `run` subcommand — produces a CodeTracer trace
  of the execution to the specified directory.
- **`--use-rust-writer` flag** — selects the Rust FFI trace writer (via
  `libcodetracer_trace_writer_ffi`) instead of the default pure-Go writer.
  Requires CGO and the FFI library from the
  [codetracer-trace-format](https://github.com/metacraft-labs/codetracer-trace-format)
  repo.
- **`--stylus` flag** — enables EVM hook functions for Arbitrum Stylus
  `debug_traceTransaction` support.
- **`tracewriter/` package** — a `TraceRecorder` interface with two
  implementations:
  - **GoWriter** — pure Go, wraps the
    [trace_record](https://github.com/metacraft-labs/trace_record) library.
    No external dependencies.
  - **RustWriter** — CGO bindings to `libcodetracer_trace_writer_ffi`. Shares
    the battle-tested Rust serialization logic used by other CodeTracer
    recorders.
- **Tracing hooks** in `internal/` — wazero internals are instrumented to
  call the `TraceRecorder` at each execution step.

The upstream wazero code is kept largely intact; tracing is injected via
hooks rather than invasive modifications, making it straightforward to
merge upstream updates.

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

### Building with the Rust FFI writer

The Rust FFI writer requires `libcodetracer_trace_writer_ffi` from the
[codetracer-trace-format](https://github.com/metacraft-labs/codetracer-trace-format)
repo. In a workspace layout where `codetracer-trace-format` is a sibling
directory, the dev shell detects it automatically:

```
metacraft/
├── codetracer-wasm-recorder/   # this repo
├── codetracer-trace-format/    # sibling — detected automatically
├── codetracer/                 # main CodeTracer repo
└── ...
```

When you enter `nix develop`, the shell hook runs
`scripts/detect-trace-format.sh` which:

1. Locates the sibling `codetracer-trace-format` repo.
2. Builds `libcodetracer_trace_writer_ffi` (if not already built).
3. Exports `CGO_ENABLED=1`, `CGO_LDFLAGS`, and `LD_LIBRARY_PATH` so that
   `go test` and `go build` link against the FFI library.

### Nix package builds

```bash
nix build                    # pure-Go wazero (no FFI)
```

To build wazero with the Rust FFI writer linked in, the consuming flake (e.g.
the main `codetracer` repo) passes a pre-built `codetracer-trace-writer-ffi`
package from `codetracer-trace-format` to `wazero.nix`.

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
# Run a WASM program and produce a CodeTracer trace:
./wazero run --out-dir ./trace-output program.wasm

# Use the Rust FFI writer (requires CGO build):
./wazero run --out-dir ./trace-output --use-rust-writer program.wasm

# Mount a directory and pass environment variables:
./wazero run --mount ./data:/data --env KEY=VALUE program.wasm

# Arbitrum Stylus debug tracing:
./wazero run --out-dir ./trace-output --stylus ./evm-hooks.so program.wasm
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
| [codetracer-rr-backend](https://github.com/metacraft-labs/codetracer-rr-backend) | Record/replay backend (rr-based) |
| [codetracer-shell-recorders](https://github.com/metacraft-labs/codetracer-shell-recorders) | Bash/Zsh recorders |

## License

This project is licensed under the Apache 2.0 License — see [LICENSE](LICENSE)
for details.

wazero is a registered trademark of Tetrate.io, Inc.
