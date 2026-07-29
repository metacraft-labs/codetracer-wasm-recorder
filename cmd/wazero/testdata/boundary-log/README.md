# `--boundary-log` test fixtures

Inputs for the M37 boundary-log replay suite (`cmd/wazero/boundary_log_test.go`
and `internal/boundarylog/*_test.go`). See
`codetracer-specs/Recording-Backends/WASM-Instrumentation-Layer.md` for the
model these exercise.

## The demo recording — a real browser session

| file | what it is |
| --- | --- |
| `frontend-wasm.ct/` | **A real browser boundary recording**, copied unmodified. |
| `balance_calc.wasm` | The **original, uninstrumented** module it records. |
| `balance_calc.wasm.manifest.json` | The `ct-instrument` sidecar manifest for it. |
| `balance_calc.rs` | The module's Rust source, for reading alongside the assertions. |

These come from the cross-process origin demo in the `codetracer` repo:

```
codetracer/src/db-backend/tests/fixtures/cross_process/account-balance-with-wasm/
    frontend-wasm.ct/                                  -> frontend-wasm.ct/
    frontend/balance_calc.wasm.manifest.json           -> balance_calc.wasm.manifest.json
    wasm-src/target/wasm32-unknown-unknown/debug/balance_calc.wasm
                                                       -> balance_calc.wasm
    wasm-src/lib.rs                                    -> balance_calc.rs
```

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
source lines at all (spec §12). Regenerate both with
`regenerate.sh` in the fixture directory named above.

## Synthetic modules

Small hand-written modules, each committed as both `.wat` source and the
`wat2wasm` output. They cover boundary shapes the demo does not have.

| file | covers |
| --- | --- |
| `imports_demo.wat` / `.wasm` | Imported functions — one returning a value, one returning nothing. Drives the generic import stubs. |
| `host_state.wat` / `.wasm` | An imported memory and an imported mutable global — spec §3.3 initial state and §3.4 host mutation. |
| `hook_imports.wat` / `.wasm` | A module still carrying the instrumenter's `__codetracer` hooks, i.e. the one replay must refuse. |

Rebuild with:

```sh
for f in imports_demo host_state hook_imports; do wat2wasm $f.wat -o $f.wasm; done
```

The recordings that drive these are **not** committed. They are built in
the tests by the producer replica in
`internal/boundarylog/browser_format_test.go`, which re-implements
`browser_session.js`'s emission rules and
`browser_stream_host.rs`'s `.ct` writer.
`TestBuilderReproducesTheCommittedBrowserRecording` pins that replica
against `frontend-wasm.ct` above, so a drift in the synthetic recordings'
format fails loudly rather than quietly making the suite test a fiction.
