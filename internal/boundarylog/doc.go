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
// `record-web` receiver writes for a browser WASM session: one CTFS
// container, `<program>.ct`, whose `boundary.log` internal file is the
// boundary log in CTBL v1 — a framed binary encoding of the record sequence
// the daemon translated from the browser's events
// (`codetracer-specs/Recording-Backends/Browser-Recording-Container.md` §3,
// decoded by `ctbl.go`). The same bytes reach a `--boundary-stream -`
// consumer on stdin while the page is still running. `recording.go`
// documents how the boundary crossings are recovered from those records.
//
// The spec §3.3 host-supplied initial state and §3.4 host mutations ride in
// the same record sequence, as host-state records (`hoststate.go`).
//
// One optional sidecar refines the replay: `<module>.wasm.manifest.json`,
// the `ct-instrument` manifest whose `boundaries` table carries each edge's
// parameter and result types (spec §3, M35). Parsed by `manifest.go`. When
// present it is cross-checked against the module's own type section and a
// disagreement is a hard error; when absent the signatures are taken from
// the module, which the spec (§6) treats as sufficient.
package boundarylog
