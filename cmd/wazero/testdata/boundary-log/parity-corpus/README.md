# `parity-corpus` — four browser recordings of four modules (M45)

Everything here is a real artefact of a real headless-Chromium run driven
through the real `record-web` daemon. Nothing is hand-written or
synthesised, because the claim under test is about **what a browser
produces**, and a recording this repository generated itself could only
show that its decoder can read its own encoder.

## Why a corpus and not one module

Spec §10 states cross-modality parity as a general property: a trace
materialised from a browser boundary recording must equal one recorded by
running the same module directly under wazero, modulo timestamps. Until
M45 it was demonstrated on exactly one module.

| | DWARF | state across calls |
| --- | --- | --- |
| `balance_calc.wasm` (the §10 demo) | yes | **no** — its only export is a pure function of its two arguments |
| `internal/wasmsnapshot/testdata/grow_mem.wasm` | **no** — hand-written `.wat`, so `steps.dat` / `types.dat` / `events.dat` are zero bytes | yes |
| every module here | yes | yes |

So trace *content* was pinned on a stateless module and *state* on a
contentless one, and nothing was pinned on both at once. A pure module
cannot distinguish a working entry state from none; a DWARF-less one
compares two empty documents while looking exactly like a comparison of
two traces.

## The four modules

| Module | Exports | What it is in the corpus for |
| --- | --- | --- |
| `loop_digest` | `absorb(u32) -> u32`, six calls | Loops and a three-deep call nest (`absorb` → `fold` → `mix` → `rotate`). The loop's trip count grows with the number of calls made, so no two calls do the same work. |
| `pair_stats` | `sample_pair(u32) -> (u32, u32)`, five calls | A **multi-value** boundary: the recording carries a result *tuple*, and every layer between the module and the replay has to slice it back the same way. A one-result export cannot tell a correct slicing from an off-by-one. |
| `vault_apply` | `apply_slot(u32) -> u32`, three calls | An **imported memory** the host stages before the first call (spec §3.3) and an import that answers by writing into it (spec §3.4). The shape every Stylus contract and every `wasm-bindgen` glue layer has. Also M44b's streaming subject. |
| `tick_ledger` | `tick(u32) -> u32`, twenty-four calls | Enough quiescent points to span several snapshots and several slices. It is what M38b's fourth deliverable — seek and slice byte-identity over a module with both DWARF and state — is asserted over. |

All four carry state across exported calls, and all four are `#![no_std]`
Rust built with `-C debuginfo=2`. `#![no_std]` keeps each committed module
around 0.6 MB instead of 1.5 MB while keeping every byte of DWARF for the
module's own source, which is the part the replay turns into per-line
steps and locals.

## Files, per module

| File | What it is |
| --- | --- |
| `<name>.wasm` | The ORIGINAL, uninstrumented module — what replay is driven against (spec §6.1). |
| `<name>.wasm.manifest.json` | The `ct-instrument` sidecar, auto-discovered by `--boundary-log`. Its `boundaries` table is cross-checked against the module's own type section. |
| `<name>-with-hyphens>.ct/` | The browser recording. Named after the page's `program`, which uses hyphens where the module uses underscores. |
| `lib.rs` | The module's source, for reading. The path baked into the DWARF is the fixture's, not this one. |
| `expected.json` | What the page observed: one list of results per exported call. Ground truth for the replay. |
| `wrap.s` (`pair_stats` only) | The multi-value half of the module — see below. |

## Why the modules are committed UNCOMPRESSED here

The same four modules are also committed in the `codetracer` repo, under
`src/db-backend/tests/fixtures/wasm-parity-corpus/`, and they are
committed **there** as `.wasm.zst` — the convention `wasm-memory-calldata`
and `wasm-nan-payloads` set. That is not a style preference: `codetracer`
runs `check-added-large-files` as a pre-commit hook, whose default cap is
500 KB, so a raw ~0.6 MB module is *rejected at commit time* in that
repo. This repository's only pre-commit hook is `lint`, so the same
bytes are committable here — and, measured below, cheaper.

They are committed **here** raw, on purpose, and the reason is measured
rather than assumed. Adding this corpus to a clone of this repository and
repacking costs:

| shape | repository growth |
| --- | --- |
| raw `.wasm` (what is committed) | **+389 KB** |
| the same four as `.wasm.zst` | +852 KB |
| the same four as `.wasm.gz` | +551 KB |

Compressing them costs **2.2x more repository**, not less. Each module is
99% DWARF (`.debug_str` alone is 45%), and the four are separate builds of
four near-identical `#![no_std]` crates, so their debug sections are
nearly the same bytes. Git deltas them against each other — one 615 KB
base plus three ~86 KB deltas — and that cross-file redundancy is exactly
what a per-file compressor cannot see. Pre-compressing hands git four
incompressible, undeltifiable blobs instead.

Two more reasons the raw form is the right one *in this repo*
specifically:

* **No decompressor is available to a Go test here.** This module has
  essentially no dependencies (`go.mod` requires exactly one), which is a
  property of wazero worth keeping; the standard library has no zstd, and
  adding `klauspost/compress` would put a dependency in the module graph
  of every downstream consumer for the sake of a test fixture. Shelling
  out to a `zstd` binary would instead make the suite depend on a tool
  the repo does not otherwise need. `codetracer`'s copy has neither
  problem: its `verify.sh` is already a shell script and already runs
  `zstd`.
* **Nothing has to expand anything.** `parity_corpus_test.go` opens the
  committed file directly, so the tests run offline from a clean checkout
  with nothing but the Go toolchain.

**The duplication across the two repos is deliberate and unavoidable.**
Both repositories run their own suite over these modules, and neither
suite may depend on the other repository's working copy being checked
out. The recording alone is not enough to recover the module, and a
module built by a different toolchain would not match the recording — see
the `codetracer` copy's `.gitignore` for why these have to be *these*
builds.

## `pair_stats`, and why part of it is assembly

`rustc` cannot emit a WebAssembly function with more than one result.
Every wasm ABI it supports on stable returns an aggregate through a hidden
pointer argument, so `extern "C" fn(u32) -> (u32, u32)` compiles to
`(i32, i32, i32) -> ()` and the boundary carries no result tuple at all —
measured, not assumed. Multi-value is reachable only from the assembly
level.

So the multi-value *signature* lives in `wrap.s` (nine instructions:
call the Rust function, split its `i64` into two halves) and everything
the module computes lives in `lib.rs`, with DWARF. Every step the
materialised trace carries comes from the Rust half.

The Rust function `sample_packed` is deliberately **not** exported. An
exported function called from inside another exported function is recorded
as a *nested export crossing*, which spec §8 refuses to replay. `rustc`
exports every `#[no_mangle]` symbol and offers no way to stop it — neither
dropping `pub`, nor `.hidden` in the assembly, nor `--no-export-dynamic`,
all measured — so the fixture links through a wrapper that drops the
`--export` pairs `rustc` adds and asks for the one export it wants by
name.

## `vault_apply`, and its two host module names

The memory is `env.memory`, which is the name `rust-lld --import-memory`
emits and is not configurable. The rate lookup is `host.fetch_rate`.

The split is what makes §10 parity *checkable* on this module. Parity
needs a "record it directly with a live host" leg, and wazero's
`HostModuleBuilder` can export functions and nothing else — so a live host
that had to supply `env.memory` *and* `env.fetch_rate` would need a
synthesised provider module, and the only one in the tree is
`internal/boundarylog/provider.go`, whose behaviour is under test on the
other leg. With the two imports on different names the direct leg needs
only a memory-defining module and an ordinary `HostModuleBuilder`, and
shares no code with the replayer.

`vault_apply.ct/trace.json` also carries the M44b in-stream host-state
records (`boundary_id: "wasm-host-state"`) alongside the
`boundary_state.json` sidecar. The two must agree; `LoadRecording` refuses
a recording where they do not.

## How they were made

`codetracer/src/db-backend/tests/fixtures/wasm-parity-corpus/regenerate.sh`
is the pipeline: it builds each module for `wasm32-unknown-unknown`,
instruments it with `ct-instrument`, starts the real `record-web` daemon
and a static server, drives the page once in headless Chromium, and copies
the recording here. `verify.sh` beside it replays the committed recordings
without rebuilding anything.

## What consumes them

| Test | Property |
| --- | --- |
| `cmd/wazero/parity_corpus_test.go` — `TestVerifyParityOverTheWholeCorpus` | §10 parity over the full `ct-print --full` document, per module. |
| `cmd/wazero/parity_corpus_test.go` — `TestVerifyTheCorpusModulesCarryDwarfAndState` | The guard: non-empty `steps.dat`/`types.dat`, and one export answering differently to the same argument. |
| `internal/boundarylog/state_carrying_equivalence_test.go` | M38b's fourth deliverable, over `tick_ledger`. |
| `internal/boundarylog/stream_host_state_test.go` and `cmd/wazero/stream_host_state_cli_test.go` | M44b, over `vault_apply`. |

## One expected diagnostic

Replaying `vault_apply` prints

```
Error constructing DWARF data. Tracing will not work: decoding dwarf
section info at offset 0x0: too short
```

That is **not** about `vault_apply.wasm`, whose DWARF is complete. It
comes from the tiny module `internal/boundarylog/provider.go` synthesises
to *define* the imported memory — wazero's `HostModuleBuilder` can only
export functions, so anything else needs a real module, and that module
has no DWARF sections. Any recording with a host-supplied memory shows it.
It is noise, not a failure; the guest's trace still carries per-line steps
and locals, and `TestVerifyTheCorpusModulesCarryDwarfAndState` fails if it
does not.
