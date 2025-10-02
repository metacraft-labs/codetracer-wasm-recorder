# Code Insights

## 2025-10-02 (test-fix-tests)
- `DecodeModule` now short circuits when `dwarf.New` returns nil, preventing the prior `IndexDwarfData` nil dereference that crashed minimal modules (validated with `GOCACHE=$(pwd)/.gocache go test ./examples/basic`).
- `internal/wasmdebug` unit tests remain red: `TestIndexDwarfData_InlinedSubroutines` still sees only one inline entry and `TestDWARFLines_Line_TinyGo` panics inside `DWARFLines.DebugPositions`.
- `internal/engine/interpreter` tests fail to compile after `newOperationSet` signature change (call sites still passing two args instead of `(int, uint32, bool)`).
- `internal/sysfs` socket tests require opening TCP listeners and now fail under sandbox restrictions (`listen tcp 127.0.0.1:0: socket: operation not permitted`); treat as environment-only and instruct reruns outside the sandbox when encountered.
- Coverage gaps remain around tracing/DWARF/stylus code paths; plan to add targeted unit/integration tests for `internal/wasmdebug`, tracing hooks, and `internal/stylus` to exercise nested inlines and rendering behaviors.
