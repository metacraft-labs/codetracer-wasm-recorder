# `nan-payloads` — two browser recordings of one module (M52)

Everything here is a real artefact of a real headless-Chromium run.
Nothing is hand-written or synthesised, because the claim under test is
about **what a browser produces**, and a recording this repo generated
itself could only show that this repo's decoder can read its own encoder.

| File | What it is |
| --- | --- |
| `nan_payloads.wasm` | The ORIGINAL, uninstrumented module — what replay is driven against (spec §6.1). Debug build, DWARF intact. |
| `nan_payloads.wasm.manifest.json` | The `ct-instrument` sidecar, auto-discovered by `--boundary-log`. Its `boundaries` table declares the `f32` / `f64` edges and is cross-checked against the module's own type section. |
| `lib.rs` | The module's source, for reading. The path baked into the DWARF is the fixture's, not this one. |
| `nan-payloads.ct/` | The browser recording made with the **M52** producer. |
| `legacy-encoding.ct/` | The same page, same module, same browser, recorded with the **pre-M52** producer. The negative control. |

## How they were made

`codetracer/src/db-backend/tests/fixtures/wasm-nan-payloads/regenerate.sh`
is the pipeline: it builds `wasm-src/` for `wasm32-unknown-unknown`,
instruments it with `ct-instrument`, starts the real `record-web` daemon
and a static server, and drives the page once in headless Chromium. The
recording it writes is copied here.

`legacy-encoding.ct/` was produced by the identical run with a
`ct-instrument` built before the M52 hook-ABI change, so the page loaded
a module importing `__ct_emit_f32` / `__ct_emit_f64` and
`browser_session.js` served them through the old path.

## What the two recordings differ in

The module computes, from host-supplied integer arguments, an `f32`
signalling NaN (`0x7F800001`), an `f64` payload-carrying quiet NaN
(`0x7FF80000DEADBEEF`), an `f32` negative zero, and an `f64` negative
zero it derives by `-0.0 * 1.0`. Each crosses the boundary twice — as an
`observe_*` import argument and as the export's own result.

```
                       nan-payloads.ct        legacy-encoding.ct
f32 signalling NaN     "f32:0x7f800001"       "null"
f32 negative zero      "f32:0x80000000"       "0"
f64 payload NaN        "f64:0x7ff80000deadbeef"  "null"
f64 negative zero      "f64:0x8000000000000000"  "0"
```

The old path did not merely canonicalise the NaN: a WebAssembly float
parameter reaches JavaScript as a `Number`, whose NaN has no payload,
and `JSON.stringify(NaN)` is `null` — so there was no value left to
compare. `-0` lost only its sign, which is worse in a different way,
because it decodes to a plausible `+0.0` and replays silently wrong.

`cmd/wazero/nan_payload_test.go` asserts on both, on **bits** and never
on `==`: two NaNs with different payloads are both `NaN`, and
`-0.0 == +0.0` is true, so a float comparison here would pass on exactly
the values that are wrong.

**Do not regenerate `legacy-encoding.ct` with a current
`ct-instrument`.** It would then carry the new encoding and the negative
control would prove nothing. The test guards against that by asserting
the file still contains `"f":"null"`.
