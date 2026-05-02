# codetracer-wasm-recorder CTFS audit (2026-05-02)

This memo summarises the CTFS audit performed against
`codetracer-wasm-recorder` in iteration 1.60 of the IsoNim migration
campaign. It documents the architecture, the audit checklist outcomes,
the concrete fixes that landed in the same session, and the open
follow-ups that are out of scope for a single recorder audit.

For the broader campaign context, see
`/tmp/isonim-migration.txt` mission goals #5 (recorder fixes) and #6
(CTFS format migration), and the cross-cutting checklist in
section 5.6 of that file.

## Architecture

The recorder is a fork of [wazero](https://wazero.io) (a pure-Go
WebAssembly runtime by Tetrate) extended with:

1. A `tracewriter` package exposing a `TraceRecorder` interface used by
   the wasm interpreter to emit canonical CodeTracer events
   (`RegisterCall` / `RegisterStep` / `RegisterReturn` / `RegisterVariable`
   / `RegisterRecordEvent`).
2. A pure-Go writer (`go_writer.go` wrapping
   `github.com/metacraft-labs/trace_record`) that emits the legacy
   three-file JSON layout (`trace.json` + `trace_metadata.json` +
   `trace_paths.json`).
3. A Rust-FFI writer (`rust_writer.go` linking against
   `codetracer_trace_writer_ffi`) that buffers events in Go and replays
   them through cgo.
4. A Stylus-trace replay layer (`internal/stylus/`) that hosts the
   `vm_hooks` host module Arbitrum Stylus contracts import, fed by an
   external EVM `debug_traceTransaction` JSON.

The wasm interpreter (`internal/engine/interpreter/interpreter.go`)
drives `RegisterCall` / `RegisterStep` / `RegisterReturn` from DWARF
function records and line records. Stylus host functions
(`internal/stylus/stylus_funcs.go`) route the 32+ EVM hooks (`emit_log`,
`call_contract`, `storage_load_bytes32`, `account_balance`, ...) through
`RegisterRecordEvent(EventKindEvmEvent, hookName, payload)` -- the same
EvmEvent routing closed for the EVM recorder in iteration 1.39.

This is the **second audited recorder that combines a Go process
(wazero) with the Rust trace writer via cgo FFI** -- the first being
the PHP recorder (Zend extension via C FFI, audited 2026-05-02 in 1.41).

## Pre-audit state (per section 5.6 checklist)

| Check | Item | Status |
|---|---|---|
| (a) | CTFS format compliance | GAP -- `rust_writer.go` hardcoded `C.FMT_JSON`; the Go writer emits its own legacy three-file JSON. |
| (b) | `register_call` for each call | OK -- DWARF-driven `RegisterCall` at every wasm function entry plus inline-entry frames. |
| (c) | `register_call_arg` via `writer.arg` | PARTIAL -- the Go side stages args via `m.Record.Arg(name, val)` correctly, but the FFI replay path in `rust_writer.go::rustEventCall` routes them through `trace_writer_register_variable_*` instead of a dedicated `register_call_arg`, so they surface as scoped variables rather than `CallRecord.args`. The C FFI does not yet expose `trace_writer_register_call_arg` -- same FFI-extension blocker as PHP 1.41. |
| (d) | `Write` / `EvmEvent` / `Error` routing | PARTIAL -- EvmEvent routing is correct in `stylus_funcs.go` (32+ Stylus host fns); no Error routing for trace mismatches or wasm trap panics; no Write routing because wazero's stdout/stderr streams already flow through the `wasi_snapshot_preview1` filesystem layer that the Go writer intercepts on its own path. |
| (e) | `register_thread_*` | N/A -- wasm core is single-threaded in the interpreter path; the wasm threads proposal is not plumbed. |
| (f) | Step records | OK -- DWARF-driven `RegisterStep` in `interpreter.go`. |
| (g) | CTFS schema match | GAP -- both writer paths produce legacy schemas. |
| (h) | Obsolete `add_event` stubs | OK -- no `add_event` in source. |
| (i) | `#[no_mangle]` collisions | OK -- this is a Go binary calling Rust via cgo; no Rust-side `#[no_mangle]` stubs in the recorder. |

## Fixes landed this iteration

### 1. Default-Ctfs CLI flag (audit a)

`cmd/wazero/wazero.go`: the `wazero run` subcommand gains a
`-format` flag (default `ctfs`) accepting `ctfs` / `binary` /
`binary_v0` / `json` / `go`. A new `resolveTraceFormat` helper centralises
the per-format dispatch and the FFI-not-yet-exposed-Ctfs error message.

This mirrors the per-recorder default-Ctfs CLI idiom landed for
Leo (1.59), Circom (1.58), TON (1.57), Miden (1.56), PolkaVM (1.55),
Fuel (1.53), Flow (1.52), Cairo (1.50), Cardano (1.48), Move (1.46),
Solana (1.44).

The Ctfs branch currently exits with a descriptive error pointing at
the FFI-extension follow-up shared with the PHP recorder. This makes
the future migration a one-line change in `resolveTraceFormat` once the
FFI exposes `FMT_CTFS` -- and crucially, **today's default behaviour
does not silently produce a legacy format**: the user has to opt in to
`-format=binary` or `-format=go` to record anything.

The legacy `-use-rust-writer` boolean is preserved for backwards
compatibility but is now incompatible with `-format=go`.

### 2. Configurable FFI format byte (audit a continuation)

`tracewriter/rust_writer.go`: replaces the hardcoded `C.FMT_JSON`
with a configurable `RustFormat` field plumbed through a new
`NewRustTraceWriterWithFormat` constructor. The pre-existing
`NewRustTraceWriter` defaults to `RustFormatJSON` to preserve the
behaviour of any external caller; the new constructor lets `cmd/wazero`
request `RustFormatBinary` (CBOR+Zstd, the closest-to-modern variant
the FFI exposes today) via `-format=binary`.

A public `RustFormat` typed enum is exported with values mirroring the
FFI header (`FMT_JSON=0`, `FMT_BINARY_V0=1`, `FMT_BINARY=2`). When the
FFI gains `FMT_CTFS` we add a new constant and a new branch in
`resolveTraceFormat`; nothing else needs to change.

`tracewriter/rust_writer_stub.go` (the non-cgo build) re-exports the
same constants so `cmd/wazero` can compile without conditional source
code at the call site.

### 3. Error special-event routing in Stylus host hooks (audit d)

`internal/stylus/stylus_funcs.go::exportFunc`: pre-fix, every Stylus
host hook panicked unconditionally on a trace-mismatch
(`trace.nextEvent(name)` failure) or on downstream wasm-memory
panics. Post-fix, both failure paths route through
`record.RegisterRecordEvent(EventKindError, "stylus_trace_mismatch" |
"stylus_host_panic", msg)` before re-raising the panic, so the partial
`.ct` container retains a breadcrumb at the failure point. Mirrors the
EVM 1.39 / Cairo 1.50 / PolkaVM 1.55 / Miden 1.56 / TON 1.57
Error-routing pattern.

The recorder is wired through a new `StylusTrace.errorRecord` field
(populated by `exportSylusFunctions` from the recorder it receives as
an argument) so the existing 34 `exportFunc` call sites do not change
shape.

### 4. Audit-pinning unit tests

`tracewriter/rust_writer_test.go`: adds three tests (one merged into
existing harness):

* `TestRustTraceWriterFormatConstants` pins `RustFormatJSON=0`,
  `RustFormatBinaryV0=1`, `RustFormatBinary=2` so a future re-cbindgen
  of `codetracer_trace_writer.h` cannot silently reorder the variants.
* `TestRustTraceWriterWithFormatBinary` smoke-tests the new
  explicit-format constructor end-to-end (event recording +
  `ProduceTrace` against a temp dir).

## Verification

```
direnv exec . go build ./cmd/wazero/                       # clean
direnv exec . go test ./tracewriter/...                    # 4 pass (3 pre-existing, 2 new under TestRustTraceWriter*, +1 const pin)
direnv exec . go test ./internal/stylus/...                # no test files
direnv exec . go test -count=1 ./cmd/wazero/...            # pass (~0.4s)
```

No regressions; no linter touches required.

## Open follow-ups (deferred; not blocking this iteration)

These are out of scope for a single recorder audit and are tracked
either at the FFI extension layer or in this recorder's future
iterations. Each follow-up describes the **fix shape** so the next
sub-agent can pick it up without re-deriving the analysis.

### A. C FFI missing `FMT_CTFS` / `register_call_arg` / `register_thread_*`

Same blocker as PHP recorder 1.41
(see `codetracer-php-recorder/AUDIT-CTFS-2026-05.md` "Open gaps"
section). Fix shape (in `codetracer-trace-format/codetracer-trace-writer-ffi`):

1. Add a `Ctfs` variant to the `Fmt` enum, dispatching to the
   multi-stream `.ct` writer.
2. Add a `trace_writer_register_call_arg(handle, name, value, ...)`
   entry point so FFI consumers can stage args on `CallRecord.args`
   instead of as scoped variables.
3. Add `trace_writer_register_thread_{start,exit,switch}` entry points.
4. Re-run `cbindgen` to regenerate the header
   `tracewriter/codetracer_trace_writer.h`; bump the `#define`
   guard / version comment so consumers can detect the new ABI.
5. Add a corresponding `RustFormatCtfs RustFormat = 3` constant in
   `rust_writer.go` (and `rust_writer_stub.go`); flip the `case "ctfs"`
   branch in `cmd/wazero/wazero.go::resolveTraceFormat` from the error
   path to `return tracewriter.RustFormatCtfs, formatKindFFI, nil`.

After this lands, the audit's (a) / (c) / (g) gaps close with no
recorder-side code change beyond the constant + dispatch flip.

### B. Live wasm function arguments via DWARF

Audit (c) on the source-level path is already closed via
`m.Record.Arg(name, val)` in
`interpreter.go::traceFunctionEntry` -- live values flow through
DWARF `FunctionRecord.Params` + `readVariable`. The downstream FFI
replay path collapses them onto `register_variable_*` because the FFI
lacks `register_call_arg` (follow-up A). Once A lands, no recorder
work is needed.

For wasm modules **without** DWARF (raw `.wasm` files compiled without
`-g`), the recorder cannot recover parameter names; placeholder
staging via `argN` per local would mirror the Miden 1.56 operand-stack
pattern (`stack[0..3]` -> `s0..s3`). Out of scope; documented for
completeness.

### C. Stylus EVM-event payload decoding

`stylus_funcs.go::exportEmitLog` writes the raw hex bytes of the
`emit_log` payload to the `EvmEvent` content. A future iteration can
parse the EVM ABI (topics + data) into a structured payload so the
GUI's events panel renders human-readable args (mirrors EVM 1.39's
"convert this to human readable format" TODO that is still open). Out
of scope for the CTFS audit.

### D. Wasm threads proposal

The wazero engine does not currently support the wasm threads
proposal. If a future iteration enables it, the recorder must call
`register_thread_start` / `register_thread_exit` /
`register_thread_switch` (FFI extension follow-up A above) at the
per-thread entry / exit points. Currently flagged N/A for this audit.

### E. Multi-stream IO event collapse

Cross-cutting issue documented in 1.39 / 1.41 / 1.44 / 1.46 / 1.48 /
1.50 / 1.52 / 1.53 / 1.55 / 1.56 / 1.57 / 1.58 / 1.59. Once the
wasm recorder routes `EvmEvent` and `Error` through the multi-stream
writer, both currently collapse onto stdout/stderr buckets and lose
the `metadata` field. Out of scope for any single recorder audit;
flagged as a writer-side fix.

### F. Read-side end-to-end content assertions

The new `TestRustTraceWriterWithFormatBinary` smoke-tests that
`ProduceTrace` runs without error. It does not walk the resulting
file and assert specific event records. Open for the wasm recorder
plus all 9+ sibling recorders (Cairo, Cardano, Flow, Fuel, PolkaVM,
Miden, TON, Circom, Leo). Adding the Nim trace-reader as a Go test
dep is the cross-cutting blocker.

## Cross-cutting findings affecting other audits

* **Two-process Go+Rust recorders.** The wasm recorder is the second
  audited cgo-based recorder (after PHP 1.41) where the Rust FFI
  surface is the binding constraint -- not the recorder logic. The
  PHP recorder's "FFI extension" follow-up (see
  `codetracer-php-recorder/AUDIT-CTFS-2026-05.md`) now blocks the wasm
  audit's (a) / (c) / (g) gaps too. Bundling these two recorders
  behind a single FFI-extension PR is the highest-leverage next step.

* **`-format` CLI default for binaries that wrap a runtime.** Unlike
  the Rust crates (Leo / Miden / TON / ...) where the recorder *is*
  the CLI, wazero is a long-lived runtime CLI with many existing
  flags and call sites. Defaulting `-format=ctfs` here required
  designing the error path to be informative-but-blocking rather than
  silently falling back, so users get a clear pointer at the FFI
  follow-up. Pattern is reusable for any future cgo recorder.

* **Stylus host-fn panic routing.** The 34-call-site `exportFunc`
  pattern is identical to the way `wazero/internal/wasi_snapshot_preview1`
  exports WASI functions. If the WASI host-fn layer ever needs Error
  routing for I/O failures, the same `errorRecord` field idiom on the
  module-builder factory is reusable.

---

Audit performed by Claude Opus 4.7 (1M context) on 2026-05-02 as part
of iteration 1.60 of the IsoNim migration campaign. See
`/tmp/isonim-migration.txt` for the full campaign log.
