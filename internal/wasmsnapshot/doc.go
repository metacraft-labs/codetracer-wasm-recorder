// Package wasmsnapshot implements WASM replay snapshots — the fast-seeking
// half of the boundary-capture model.
//
// It is specified by
// `codetracer-specs/Recording-Backends/WASM-Replay-Snapshots-And-Slices.md`
// (below: "the snapshot spec"), which builds on
// `WASM-Instrumentation-Layer.md` (the boundary-recording model implemented by
// `internal/boundarylog`) and reuses
// `Multi-Core-Recorder/MCR-Memory-Page-CAS.md` (below: "the CAS spec") for
// content addressing.
//
// # The idea
//
// Replaying a boundary recording is correct but linear: reaching a point late
// in a recording costs re-executing everything before it. This package derives
// periodic memory snapshots during that replay so any sub-range can be
// materialised on demand, exactly as MCR checkpoints do for native recordings.
//
// Four properties are load-bearing and each is enforced by code here rather
// than left to convention:
//
//  1. **Snapshots are taken at quiescent points only** (snapshot spec §3). A
//     quiescent point is a moment when no exported function is executing. The
//     WASM stack is empty there, so linear memory + globals + tables *are* the
//     entire state and no engine-internal execution context has to be
//     captured — which is what makes such a snapshot resumable by any engine.
//     `quiescent.go` identifies them from the boundary log alone.
//
//  2. **Snapshots are derived data.** They are fully reconstructible from
//     `(module + boundary log)` and may be discarded and re-derived at a
//     different density without re-recording. Nothing in this package is a
//     source of truth.
//
//  3. **Snapshots live inside the `.ct` container** as additive CTFS
//     namespaces (snapshot spec §6), never as sidecar files. `container.go`
//     appends them through `internal/ctfs`.
//
//  4. **A `.ct` without the snapshot namespaces is a complete recording, and
//     an unreadable snapshot version disables seeking and nothing else**
//     (snapshot spec §6, last paragraph). This is a deliberate narrowing of
//     the CAS spec §10 rule, where an unknown `cas_version` rejects the whole
//     trace: here the snapshot streams are disjoint from the boundary streams,
//     so refusing the container would make a recording unreadable over a
//     component that only ever makes it faster. `Load` therefore returns a
//     *diagnostic*, not an error, for an unrecognised version.
//
// # Build split
//
// Snapshot **derivation** is commercial and gated behind the `ctsnapshots`
// build tag (snapshot spec §9, "Packaging": two artifacts from one source
// tree). Snapshot **reading** is open and unconditional. See `derive.go` /
// `derive_disabled.go`, and `container_open_build_test.go` for the invariant
// test that the open build materialises a snapshot-bearing container.
package wasmsnapshot
