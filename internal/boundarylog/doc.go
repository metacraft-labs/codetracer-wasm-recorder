// Package boundarylog implements the consumer half of the CodeTracer WASM
// "boundary-capture" recording model: it takes a *boundary recording*
// produced by the browser pipeline plus the **original, uninstrumented**
// `.wasm` and re-executes the module under wazero so the interpreter can
// materialise a full CTFS trace.
//
// The model, and every rule this package implements, is specified in
// `codetracer-specs/Recording-Backends/WASM-Instrumentation-Layer.md`
// (referred to below simply as "the spec"). The short version:
//
//   - A WebAssembly module is deterministic given its imports (spec §7),
//     so the browser records only what crosses the module/host boundary
//     (spec §3) and not what happens inside the module (spec §4).
//   - Replay instantiates the original module, feeds every import the
//     recorded results instead of calling a real host, invokes each
//     recorded exported call with its recorded arguments, and lets the
//     interpreter's DWARF stepping produce the step/call/variable events
//     (spec §6).
//   - **Divergence is an error, never a warning** (spec §6). A mismatched
//     import index, a mismatched argument, or a mismatched exported return
//     value aborts the replay naming the recorded-vs-actual pair, and no
//     trace is written.
//
// This is the same shape `internal/stylus` implements for Arbitrum Stylus,
// generalised from one hard-coded `vm_hooks` host module and one JSON
// schema to any module's imports driven by a recorded boundary log. See
// `AGENTS.md` ("Boundary-log replay vs. Stylus replay") for why the two
// paths remain separate.
//
// # Input format
//
// A boundary recording is what the CodeTracer backend-manager's
// `record-web` receiver writes for a browser WASM session: a `.ct`
// **directory** in the legacy three-file JSON trace layout —
//
//	<program>.ct/trace.json           []TraceLowLevelEvent
//	<program>.ct/trace_metadata.json  {program, args, workdir, recorder}
//	<program>.ct/trace_paths.json     []string
//
// — written by `codetracer/src/backend-manager/src/browser_stream_host.rs`
// from the newline-delimited JSON stream that
// `codetracer-wasm-instrumenter/recorder-runtime/browser_session.js` sends
// over a WebSocket. `recording.go` documents how the boundary crossings are
// recovered from that rendering.
//
// Two optional sidecars refine the replay:
//
//   - `<module>.wasm.manifest.json` — the `ct-instrument` sidecar manifest
//     whose `boundaries` table carries each edge's parameter and result
//     types (spec §3, M35). Parsed by `manifest.go`. When present it is
//     cross-checked against the module's own type section and a
//     disagreement is a hard error; when absent the signatures are taken
//     from the module, which the spec (§6) treats as sufficient.
//   - `boundary_state.json` inside the `.ct` directory — the spec §3.3
//     host-supplied initial state and §3.4 host mutations. Parsed by
//     `hoststate.go`. See that file for the schema and for an explicit
//     statement of which producer emits it today.
package boundarylog
