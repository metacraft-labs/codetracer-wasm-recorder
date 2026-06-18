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
file and assert specific event records.

This is now explicitly **blocked by follow-up A** for the wasm/wazero
recorder. The currently supported Rust FFI formats are JSON,
BinaryV0, and Binary; none of them produce the canonical multi-stream
CTFS `.ct` container consumed by `NimTraceReaderHandle`. The available
binary path is the legacy CBOR+Zstd format produced through the same
three-phase `trace.json` / `trace_metadata.json` / `trace_paths.json`
FFI calls, so adding a Nim-reader test today would only prove that the
reader cannot open the expected format.

Once follow-up A adds `FMT_CTFS`, the minimal recorder-side test shape
is:

1. Add `RustFormatCtfs` in `rust_writer.go` / `rust_writer_stub.go`
   and route `-format=ctfs` through `NewRustTraceWriterWithFormat`.
2. Extend `tracewriter/rust_writer_test.go` (or add a nearby cgo-only
   test) to record a tiny function with one step, one call, one staged
   argument, and one return using `RustFormatCtfs`.
3. Open the produced `.ct` container through the Nim CTFS reader and
   assert concrete content: function name, source path/line, the step,
   and `Call.args[]`.
4. Add a Stylus-host mismatch/panic fixture only if the FFI test already
   proves the basic call/step/return stream is reader-visible; then
   assert the `Error` special event content.

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

---

## Convention compliance follow-up — 2026-05-08

Iteration 1.60 left the `-format` CLI flag in place to allow a graceful
opt-in to `binary` / `json` / `go` writers while the FFI exposed neither
`FMT_CTFS` nor a Nim-backed CTFS variant.  The cross-cutting recorder
convention pass (Python, Ruby, JavaScript, Cairo, Cardano, Circom, Flow,
Fuel, Leo, Miden, Move, PolkaVM, Solana, TON, Bash, Zsh, EVM, Native) has
since standardised the recorder CLI surface on **CTFS-only output with
no `--format` flag** (`Recorder-CLI-Conventions.md` §4) and on
per-recorder env-var fallbacks (§5).  This follow-up brings the wasm
recorder onto that contract.

### Binary-name exception (preserved)

`Recorder-CLI-Conventions.md` §1 documents the wasm recorder as the
**one exception** to the `codetracer-<lang>-recorder` binary-name rule:
the recorder is a fork of Tetrate's [wazero](https://github.com/tetratelabs/wazero)
runtime with tracing layered in, not a CodeTracer-named tool.  The binary
keeps the upstream `wazero` name; the convention's other contracts
(`--out-dir`, env vars, no `--format`, `ct print` mention) apply
unchanged.  See `Recorder-CLI-Conventions.md` Implementation Status table
for the canonical phrasing ("✓ Compliant (CTFS-only, binary name
exception)").

### Changes landed this follow-up

1. **`-format` flag removed** (`cmd/wazero/wazero.go`).  The
   `resolveTraceFormat` helper, the `formatKind` enum, and the
   `-use-rust-writer` boolean all went with it.  Today the recorder
   resolves `--out-dir` plus the new env-var fallback into a single
   pinned `tracewriter.GoWriter` instance.  When the FFI/Nim CTFS writer
   lands the dispatch switches to it without changing the CLI surface.

2. **`CODETRACER_WASM_RECORDER_OUT_DIR`** is now read as a fallback for
   `--out-dir` (`cmd/wazero/wazero.go`, `doRun`).  Mirrors §5 across the
   recorder fleet.

3. **`CODETRACER_WASM_RECORDER_DISABLED`** short-circuits recording
   entirely.  When set, the recorder runs the target through wazero
   without instantiating a `TraceRecorder`; no trace artefacts are
   written.  Mirrors §5 across the recorder fleet.

4. **Rust FFI writer removed** (`tracewriter/rust_writer.go`,
   `rust_writer_stub.go`, `rust_writer_test.go`,
   `tracewriter/codetracer_trace_writer.h`).  The FFI writer was the
   only consumer of the `--format` flag and produced legacy
   non-CTFS shapes.  It can be re-added if and when the FFI exposes
   `FMT_CTFS` (open follow-up A above), without touching call sites.

5. **Help-text updates** — `wazero -h` and `wazero run -h` now mention
   the env vars and `ct print` from `codetracer-trace-format-nim` as the
   canonical conversion tool.  The legacy `--format` flag is no longer
   advertised; the `flag` package rejects it with the standard
   "flag provided but not defined" error.

6. **Tests** (`cmd/wazero/wazero_test.go`):
   * `TestNoFormatFlagInHelp` — sweeps top-level / `run` / `compile`
     `-h` to ensure no `--format` / `CODETRACER_FORMAT` advertisement.
   * `TestHelpMentionsCtPrint` — both top-level and `run -h` must
     mention `ct print`.
   * `TestFormatFlagRejected` — `wazero run --format json fixture.wasm`
     must exit non-zero (re-execs the test binary as a subprocess
     because `flag.ExitOnError` calls `os.Exit(2)` on the unknown flag).
   * `TestEnvOutDirUsedWhenFlagOmitted` — exercises the
     `CODETRACER_WASM_RECORDER_OUT_DIR` fallback, asserting the
     three-file JSON layout lands in the env-supplied dir.
   * `TestEnvDisabledSkipsRecording` — exercises the
     `CODETRACER_WASM_RECORDER_DISABLED=1` short-circuit, asserting no
     trace artefacts are written.
   * `TestRecordedTraceViaCtPrintJson` — records a tiny WASI fixture
     and pipes the resulting bundle through `ct-print --json`,
     asserting on **structural anchors** (`metadata` / `paths` /
     `functions` / `steps` / `calls` / `ioEvents` section names) rather
     than on integer values that don't round-trip today.  Skips
     gracefully when ct-print is not present (i.e. when this repo is
     built outside the metacraft workspace).  Mirrors the pattern landed
     for cardano / circom / flow / fuel / leo / miden / move /
     polkavm / python / ruby / solana / ton.

7. **`tests/verify-cli-convention-no-silent-skip.sh`** — shell guard
   that asserts:
   * `--format` / `CODETRACER_FORMAT` absent from `wazero -h`,
     `wazero run -h`, `wazero compile -h`.
   * `--out-dir`, `ct print`, both env-var names present in
     `wazero run -h`.
   * `CODETRACER_WASM_RECORDER_OUT_DIR` /
     `CODETRACER_WASM_RECORDER_DISABLED` referenced in `cmd/`.
   * `tracewriter/rust_writer.go` no longer exists (catches a partial
     revert).
   Wired into the `Justfile` (`just verify-cli-convention`,
   `just check-all`).

8. **`README.md`** — updated to advertise the env-var fallback, the
   binary-name exception, and the `ct print` conversion workflow; the
   "Building with the Rust FFI writer" section was replaced with a brief
   workspace-layout note.

9. **`Recorder-CLI-Conventions.md`** Implementation Status row flipped
   from `⚠ Partial` to
   `✓ Compliant (CTFS-only, binary name exception)`.

### Verification

```
direnv exec . go build ./cmd/wazero/       # clean
direnv exec . go test -count=1 ./cmd/wazero/   # all CLI tests, including the
                                                # new no-silent-skip set
direnv exec . go test -count=1 ./tracewriter/  # CTFS writer test set
direnv exec . bash tests/verify-cli-convention-no-silent-skip.sh  # all assertions hold
```

Open follow-up A (FFI extension exposing `FMT_CTFS` /
`register_call_arg` / `register_thread_*`) remains the path to a real
multi-stream `.ct` container; today the recorder's pinned Go writer
emits the legacy three-file JSON layout that ct-print also accepts.  The
open follow-ups B–F from the 2026-05-02 audit are unchanged.

## Follow-up A landed — 2026-05-08 (CTFS FFI migration)

The deferred follow-up A from above closed in the same iteration as the
convention compliance pass:

- `tracewriter.GoWriter` (legacy three-file JSON) was replaced with
  `tracewriter.CtfsTraceWriter`, a cgo binding to
  `codetracer-trace-format-nim/src/codetracer_trace_writer_ffi.nim`.
  The Nim FFI maps `FFI_TRACE_FORMAT_BINARY` (= 2) onto its
  multi-stream `MultiStreamTraceWriter`, producing a single
  `<program-basename>.ct` (CTFS) container under `--out-dir` — no
  legacy fallback files are emitted.
- Header `tracewriter/codetracer_trace_writer.h` is the verbatim copy
  of `codetracer-trace-format-nim/include/codetracer_trace_writer.h`;
  the cgo binding links statically against
  `libcodetracer_trace_writer.a` plus `-lzstd -lm -lpthread`.
- Typed value variants surface correctly: `IntValueRecord` →
  `kind: "Int"` (via `trace_writer_register_variable_int` /
  `trace_writer_register_return_int`), `StringValueRecord` →
  `kind: "String"` (via the streaming CBOR encoder
  `ct_value_write_string` + `trace_writer_register_variable_cbor`),
  `StructValueRecord` → `kind: "Struct"` (via
  `ct_value_begin_struct` + `ct_value_end_compound`).  The deferred
  Skipf in `TestRecordedTraceViaCtPrintJson` was retired and the strict
  exact-value assertions now run unconditionally.
- The `nix develop` shell hook `scripts/detect-trace-format.sh` was
  rewritten to find the `codetracer-trace-format-nim` sibling, build
  `libcodetracer_trace_writer.a` via `nimble buildLib` if missing, and
  export `CGO_CFLAGS` / `CGO_LDFLAGS` plus `LD_LIBRARY_PATH` for both
  the Nim FFI and `libzstd`.  `wazero.nix` accepts a
  `codetracer-trace-format-nim` derivation (replacing the previous
  `codetracer-trace-writer-ffi` input) for production builds.

Open follow-ups B–F from the 2026-05-02 audit are unchanged.  The
`register_call_arg` / `register_thread_*` half of follow-up A is now
exposed by the Nim FFI but the wazero recorder does not yet emit
arguments live (they still surface as scoped variables) — that's a
recorder-side improvement tracked alongside follow-up B.
