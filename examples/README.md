# Recorder examples

Small WebAssembly programs (Rust, Go, or anything else that targets
`wasm32-wasip1`) that you can record with the CodeTracer wasm recorder
and replay in the CodeTracer time-travel debugger.

The fixtures under [`recorder-golden/`](recorder-golden) double as the
golden inputs for `cmd/wazero/recorder_golden_test.go`, so they stay
small and stable — perfect starting points for exploring how the
recorder maps WASM execution back to source-level steps, calls, locals,
and panics.

## Prerequisites

- **`ct`** on your `PATH` — the CodeTracer CLI (see the main
  [`codetracer`](https://github.com/metacraft-labs/codetracer) repo).
  All commands below use `ct`; the recorder binary is invoked
  transparently by `ct`.
- The **wasm recorder built** — `just build` from the repo root, or any
  `nix build` invocation that produces a recording-capable wazero
  binary (see the top-level [README](../README.md#building)).
- **The `.wasm` built.** `recorder-golden/` ships Rust sources, not
  binaries — a checked-in `.wasm` is an unverifiable claim that it
  still matches the `.rs` beside it. Build them first:

  ```bash
  bash examples/recorder-golden/build.sh
  ```

  This needs a `rustc` with the `wasm32-wasip1` target. The repo's Nix
  dev shell pins one, so inside `direnv exec . bash` (or `nix develop`)
  it just works; outside Nix, `rustup target add wasm32-wasip1` first.

  The script compiles each fixture with full DWARF debug info
  preserved — that part is load-bearing, not a preference, because
  without it the recorder has no line table to map PC offsets back to:

  ```bash
  rustc -g -C debuginfo=2 -C opt-level=0 \
        --edition 2021 \
        --target wasm32-wasip1 \
        -o column_aware.wasm column_aware.rs
  ```

  The Go test suite does not use these copies; it compiles its own from
  `cmd/wazero/testdata/recorder-golden/*.rs` as it runs.

## Two-step workflow: record, then replay

Produce a `.ct` trace bundle, then open it in the GUI as a separate
step:

```bash
# 1. Record. The output is a `<basename>.ct` bundle in the working dir.
ct record examples/recorder-golden/column_aware.wasm

# 2. Replay. Pass the trace bundle with -t; ct replay launches the GUI.
ct replay -t ./column_aware.ct
```

Use this flow when you want to keep the recorded trace around for
later analysis, share it with someone else, or feed it to headless
tooling before opening it visually.

## One-step workflow: record and open

```bash
ct run examples/recorder-golden/column_aware.wasm
```

`ct run` records the program and opens the resulting trace in the
CodeTracer GUI in a single command — handy for the inner-loop
"tweak the program, look at the trace" cycle.

## Walkthrough: `column_aware.rs`

[`recorder-golden/column_aware.rs`](recorder-golden/column_aware.rs)
is a deliberately tiny fixture that highlights **column-aware
step-over** — the recorder's ability to distinguish individual
statements that share a single source line.

The interesting line is line 17:

```rust
let a: i32 = 1; let b: i32 = 2; let c: i32 = 3;
```

Three `let` bindings live on one line, each starting at a distinct
1-based column. The recorder must emit a separate step event for each
statement, with a strictly increasing column value (the acceptance
criterion behind FU-Column-Aware-Nav-Wasm — see
`codetracer-specs/Planned-Features/Column-Aware-Navigation-Other-Languages.plan.md`).

Try it end-to-end:

```bash
ct run examples/recorder-golden/column_aware.wasm
```

In the CodeTracer GUI, step into `three_on_one_line` and step-over
from the start of line 17. Instead of jumping straight to line 18, the
debugger stops three times on line 17 — once at each `let` binding —
before advancing. Local-variable values appear in the locals panel as
each binding is assigned. That column granularity is what
"column-aware step-over" buys you.

The other fixtures explore complementary surface area:

| Fixture | What it exercises |
|:--------|:------------------|
| [`control_flow.rs`](recorder-golden/control_flow.rs)   | `if`/`else`, `for`, nested `while` — step ordering and loop iteration |
| [`nested_calls.rs`](recorder-golden/nested_calls.rs)   | 3-deep call chain plus recursion — `call_entry` / `call_exit` pairing |
| [`collections.rs`](recorder-golden/collections.rs)     | `Vec`, `HashMap`, structs, tuples — `Sequence` / `Struct` / `Tuple` value records |
| [`panic_path.rs`](recorder-golden/panic_path.rs)       | Explicit `panic!` — recorder emits an `Error`-kind event on trap |

Record any of them the same way as `column_aware.wasm`.

## Other examples in this directory

The remaining subdirectories are the upstream wazero examples,
preserved here for embedding/runtime usage and unrelated to the
recorder:

- [`allocation`](allocation) — pass strings in and out of WebAssembly
- [`basic`](basic) — mix host-defined and WebAssembly-defined functions
- [`cli`](cli) — wire wazero into a command-line tool
- [`concurrent-instantiation`](concurrent-instantiation) — multiple Wasm instances per goroutine
- [`import-go`](import-go) — call a Go-defined function from WebAssembly
- [`multiple-results`](multiple-results) — return more than one result
- [`multiple-runtimes`](multiple-runtimes) — share compilation caches
