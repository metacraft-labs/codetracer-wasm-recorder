# `--boundary-log` test fixtures

Inputs for the M37 boundary-log replay suite (`cmd/wazero/boundary_log_test.go`
and `internal/boundarylog/*_test.go`). See
`codetracer-specs/Recording-Backends/WASM-Instrumentation-Layer.md` for the
model these exercise.

## The rule these files live under

Everything here is a **captured vector of a producer this repo does not
build**. That is the only reason a recording is committed anywhere in this
workspace.

The sibling `codetracer` repo deleted its committed recordings: it *owns*
the browser recording pipeline, so a committed recording of it was a cache
of the code under test, and its tests kept passing about a recorder that
had been replaced. Its tests now record when they run
(`codetracer/scripts/materialize-recording.sh`).

This repo is on the other side of that boundary. It is the replayer. The
pipeline that makes these recordings — `ct-instrument`, the `record-web`
daemon, headless Chromium — lives in two siblings, and `go test ./...` must
not need either. Producing them here is not available; generating them with
`internal/boundarylog`'s own `recordingBuilder` would replace the browser
with this repo's model of it, which would gut
`TestVerifyCrossModalityParity` (it would compare wazero against a Go
re-implementation of four Rust modules) and empty out
`nan_payload_test.go`'s bit-pattern assertions (they would show only that
the builder writes what it was told). So they stay.

**What changed is that the capture is now checked.** The sibling's
`wasm-parity-corpus/regenerate.sh` used to end by copying each fresh
recording straight into this directory. That made the two repos agree by
construction, and the agreement then held after the producer moved, because
the last sync had frozen it — the same defect the committed recordings had,
in cross-repo form. That step is gone. In its place:

```
just verify-vectors     # scripts/verify-vectors-against-producer.sh
```

records the same demos from the sibling's current tree and compares what
the two recordings *mean* — every recovered crossing, its kind, name,
index, depth, argument and result values, and the `MarkersIdentifyImports`
format witness. Not bytes: `trace_metadata.json` carries the absolute
directory the run happened in, so byte equality is unachievable and a check
that demanded it would be switched off within a week. The comparison lives
in `internal/boundarylog/vector_freshness_crossrepo_test.go` behind the
`crossrepo` build tag, so `just test` stays standalone.

**Never hand-edit a file under this directory, and never copy one in.** If
`just verify-vectors` fails, the producer changed; re-capture deliberately
and read the diff, because it is telling you something about the replayer
you maintain.

The single exception is `nan-payloads/legacy-encoding.ct`, which was made
by a `ct-instrument` built before M52 and cannot be produced from any
current tree. It is the one recording here that is committed for the
reason a recording *should* be committed — it is evidence about a version
that no longer exists — and it is deliberately excluded from
`verify-vectors`. See `nan-payloads/README.md`.

## The demo recording — a real browser session

| file | what it is |
| --- | --- |
| `frontend-wasm.ct/` | **A real browser boundary recording**, copied unmodified. |
| `balance_calc.wasm` | The **original, uninstrumented** module it records. |
| `balance_calc.wasm.manifest.json` | The `ct-instrument` sidecar manifest for it. |
| `balance_calc.rs` | The module's Rust source, for reading alongside the assertions. |

These were captured from the cross-process origin demo in the `codetracer`
repo. The demo's sources are still there; its recordings are not committed
any more, so the left-hand column below is what
`codetracer/scripts/materialize-recording.sh cross-process-three-trace`
produces:

```
<recording dir>/frontend-wasm.ct/                      -> frontend-wasm.ct/
codetracer/src/db-backend/tests/fixtures/cross_process/account-balance-with-wasm/
    frontend/balance_calc.wasm.manifest.json           -> balance_calc.wasm.manifest.json
    wasm-src/target/wasm32-unknown-unknown/debug/balance_calc.wasm
                                                       -> balance_calc.wasm
    wasm-src/lib.rs                                    -> balance_calc.rs
```

`just verify-vectors` is what keeps `frontend-wasm.ct/` an accurate capture.

`frontend-wasm.ct/` is the output of the real pipeline end to end: the
instrumented module ran in a browser under
`codetracer-wasm-instrumenter/recorder-runtime/browser_session.js`, which
streamed newline-delimited JSON over a WebSocket to the backend-manager's
`record-web` receiver
(`codetracer/src/backend-manager/src/browser_stream_host.rs`), which wrote
the three-file `.ct` directory. Nothing in it was authored by hand.

It records exactly one export crossing:

```
compute_balance(arg0 = 42, arg1 = 100) -> ret0 = 620
```

plus the two `js-wasm-realm` correlation markers that pair it with the
page's own JS recording. One path (`wasm-src/lib.rs`), one function
(`compute_balance`, line 71), three steps, no store events.

**Note which `.wasm` is here.** `balance_calc.wasm` is the *raw*
`cargo build --target wasm32-unknown-unknown` output, **not** the
instrumented module the browser actually ran. That is the point: spec §6.1
says replay instantiates the original module, because the interpreter
observes execution from the inside and the rewrite is a recording-time
device only. `TestVerifyUninstrumentedModuleIsUsed` asserts this file
mentions neither `__codetracer` nor `__ct_emit_call`.

The `dev` (not `release`) profile matters: release LTO discards the
per-unit DWARF line programs, and a module built that way materialises no
source lines at all (spec §12). Re-capture both from the sibling's
`regenerate.sh` (or from what `materialize-recording.sh` leaves in its
cache) when `just verify-vectors` says the producer has moved.

## Synthetic modules

Small hand-written modules, each committed as both `.wat` source and the
`wat2wasm` output. They cover boundary shapes the demo does not have.

| file | covers |
| --- | --- |
| `imports_demo.wat` / `.wasm` | Imported functions — one returning a value, one returning nothing. Drives the generic import stubs. |
| `host_state.wat` / `.wasm` | An imported memory and an imported mutable global — spec §3.3 initial state and §3.4 host mutation. |
| `hook_imports.wat` / `.wasm` | A module still carrying the instrumenter's `__codetracer` hooks, i.e. the one replay must refuse. |
| `void_import.wat` / `.wasm` | An import whose signature is `() -> ()` — the crossing that carries no boundary values at all, and whose realm markers are its whole trace on disk (M39). |

Rebuild with:

```sh
for f in imports_demo host_state hook_imports void_import; do wat2wasm $f.wat -o $f.wasm; done
```

The recordings that drive these are **not** committed, with one exception
below. They are built in the tests by the producer replica in
`internal/boundarylog/browser_format_test.go`, which re-implements
`browser_session.js`'s emission rules and
`browser_stream_host.rs`'s `.ct` writer.
`TestBuilderReproducesTheCommittedBrowserRecording` pins that replica
against `frontend-wasm.ct` above, so a drift in the synthetic recordings'
format fails loudly rather than quietly making the suite test a fiction.

## `void-import.ct` — the one committed synthetic recording

`void-import.ct/` records one `ping_n(2)` call against `void_import.wasm`:
one exported call and two crossings into `host_ping`, each carrying no
values whatsoever.

It is committed because `cmd/wazero/boundary_log_test.go` needs it and
cannot build it. That suite is `package main`, and the producer replica is
a `_test.go` file in `package boundarylog`, so it is not importable. The
CLI is the only place the "a diverged replay writes no trace" policy can be
observed at all — `internal/boundarylog` replays with a nil recorder and
writes nothing either way — so the recording has to be on disk.

A committed copy drifts, so it is not left on trust:
`internal/boundarylog/void_import_fixture_test.go` rebuilds it from the
same replica every run and asserts the bytes match
(`TestCommittedVoidImportRecordingMatchesTheReplica`). Regenerate it by
running that test's builder — `voidImportRecording()` there is the single
definition of its content.
