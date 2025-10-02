# Stabilize Test Suite After DWARF/engine refactors

## Current Failures
- `go test ./...` panics across many packages (runtime, examples, integration) because `internal/wasmdebug.IndexDwarfData` dereferences a nil `*dwarf.Data` when modules lack debug info. Related tests include `wazero.TestCompilationCache`, `cmd/wazero.TestCompile`, examples under `examples/*`, and spectest suites.
- `internal/wasmdebug` unit tests now either panic (`TestDWARFLines_Line_TinyGo`) or observe mismatched inlined record counts (`TestIndexDwarfData_InlinedSubroutines`) due to the new indexing assumptions.
- `internal/engine/interpreter` package no longer compiles its tests: `newOperationSet` gained a `localIndex` parameter but call sites in `compiler_test.go` still pass the old two-argument form.
- `internal/engine/wazevo/backend/isa/amd64.TestAdjustClonedStack` fails because pointer adjustment math no longer matches expectations after the recent stack cloning changes.
- `internal/sysfs` TCP listener tests fail in the sandbox with `socket: operation not permitted`; treat these as environment limitations rather than functional bugs.

## Remediation Plan

1. **Harden DWARF indexing for missing data** ✅ _Completed 2025-10-02_
   - Updated `internal/wasm/binary/decoder.go` to honor `dwarf.New` errors and skip DWARF indexing when construction returns `nil`.
   - Guarded `internal/wasmdebug.IndexDwarfData` against nil `*dwarf.Data` and added checks for empty location expressions alongside frame-base validation.
   - Added regression coverage in `internal/wasm/binary` and `internal/wasmdebug` ensuring DWARF-free modules decode without panics.
   - Verified via `GOCACHE=$(pwd)/.gocache go test ./internal/wasmdebug` (remaining failures limited to existing inline/TinyGo expectations) and `GOCACHE=$(pwd)/.gocache go test ./examples/basic`.

2. **Restore DWARF feature expectations**
   - Investigate why `TestIndexDwarfData_InlinedSubroutines` only sees one inline entry; adjust the indexing logic (likely in `indexInlinedEntry` or its range handling) so nested inlines are preserved.
   - Fix `TestDWARFLines_Line_TinyGo` by making `indexVariable` robust to empty location encodings and by covering the TinyGo-produced debug info layout with explicit test data.
   - Extend tests to assert the corrected behavior, ensuring the new guard paths do not mask legitimate DWARF data.

3. **Unblock interpreter compiler tests**
   - Update every `newOperationSet` call in `internal/engine/interpreter/compiler_test.go` to pass the new `localIndex` argument.
   - Audit other helper invocations (e.g., `local.tee` expectations) for the same signature change so the package compiles.
   - Run `go test ./internal/engine/interpreter` to confirm the helper adjustments are sufficient.

4. **Fix amd64 stack cloning adjustments**
   - Instrument `AdjustClonedStack` (e.g., with temporary logging or targeted assertions) to understand the off-by-diff reported by `TestAdjustClonedStack` and align the pointer arithmetic with the new stack layout.
   - Once corrected, reinforce the unit test with additional assertions covering multiple frame depths to prevent regressions.

5. **Map tracing coverage gaps**
   - Review `internal/wasmdebug`, runtime tracing hooks, and the `trace_record` integration path to catalog untested branches; produce a checklist per package noting missing scenarios (e.g., absent DWARF vs full debug info, multi-module traces).
   - Establish baseline coverage numbers (`go test -cover ./internal/wasmdebug ./experimental/...`) so improvements can be measured.

6. **Test DWARF variable readers**
   - Expand coverage for `indexVariable` and related helpers in `internal/wasmdebug` by constructing fixtures with locals, parameters, and direct memory locations.
   - Add table-driven tests that cover DW_OP_fbreg, abstract origin fallback, and malformed location encodings, asserting graceful degradation.
   - Ensure variable lookup results integrate with tracing consumers (e.g., locals and inlined scopes).

7. **Add DWARF regression tests**
   - Implement unit tests that exercise DWARF indexing edge cases (nested inlines, empty location expressions, unsupported opcodes).
   - Validate guards from milestones 1–2 by decoding modules without DWARF data and asserting no panics plus correct fallback behavior.
   - Track coverage for `internal/wasmdebug` and related packages with `go test -cover`.

8. **Add tracing regression tests**
   - Introduce tests covering runtime tracing hooks and `trace_record` integration, including minimal modules, multi-module traces, and error propagation.
   - Verify trace metadata generation stays stable when DWARF data is present or absent.
   - Re-run targeted packages with coverage flags to confirm improvements and guard against regressions.

9. **Exercise Stylus rendering paths**
   - Create new scenario tests in `internal/stylus` covering varied instruction sequences, error handling, and formatting options.
   - Add an integration test that wires stylus output into tracing or diagnostics, ensuring stylistic artifacts align with expectations.
   - Track coverage for the stylus package (`go test -cover ./internal/stylus`).

10. **Handle sandbox-only failures**
   - When tests fail solely due to sandbox restrictions (e.g., TCP bind permission errors), surface a clear message instructing the user to rerun the affected package outside the sandbox instead of modifying the tests.

11. **Full regression sweep**
   - After applying the above fixes, run the targeted suites (`go test ./...` with `GOCACHE=$(pwd)/.gocache`) to ensure no remaining panics or compilation errors.
   - Capture any residual failures for follow-up (e.g., CLI exit codes) and update `.agents/code-insights.md` once the suite passes.
