# Code Insights

## 2025-10-02 (nix-go-124)
- Dev shell pins `pkgs.go_1_24` in `shell.nix` to work around Go 1.25 regressions seen in milestone 4; revisit once upstream fixes land and update roadmap entry accordingly.

## 2025-10-02 (test-fix-tests)
- `DecodeModule` now short circuits when `dwarf.New` returns nil, preventing the prior `IndexDwarfData` nil dereference that crashed minimal modules (validated with `GOCACHE=$(pwd)/.gocache go test ./examples/basic`).
- `internal/wasmdebug` inline and TinyGo regressions are resolved by allowing point-interval inserts and guarding missing call-site metadata; `GOCACHE=$(pwd)/.gocache go test ./internal/wasmdebug` now passes.
- `internal/engine/interpreter` tests pass again after updating `newOperationSet` expectations and guarding tracing helpers for functions without DWARF offsets (`GOCACHE=$(pwd)/.gocache go test ./internal/engine/interpreter`).
- `internal/sysfs` socket tests require opening TCP listeners and now fail under sandbox restrictions (`listen tcp 127.0.0.1:0: socket: operation not permitted`); treat as environment-only and instruct reruns outside the sandbox when encountered.
- Coverage gaps remain around tracing/DWARF/stylus code paths; plan to add targeted unit/integration tests for `internal/wasmdebug`, tracing hooks, and `internal/stylus` to exercise nested inlines and rendering behaviors.

## 2025-10-02 (go test amd64 stack)
- Under Go 1.24, `GOCACHE=$(pwd)/.gocache go test ./internal/engine/wazevo/backend/isa/amd64` now passes (Milestone 4 regression no longer reproduced).
- The mismatch persists on Go 1.25, so keep the dev shell pinned to 1.24 until upstream resolves the issue.
