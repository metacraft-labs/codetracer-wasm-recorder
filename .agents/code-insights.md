# Code Insights

- 2025-10-02: `go test ./...` currently panics widely because `internal/wasmdebug.IndexDwarfData` assumes non-nil DWARF data; minimal modules (examples, spectests, integration benches) now trigger nil dereference during module decoding.
- 2025-10-02: `internal/engine/interpreter` tests fail to compile after `newOperationSet` signature change (call sites still passing two args instead of `(int, uint32, bool)`).
- 2025-10-02: `internal/sysfs` socket tests require opening TCP listeners and now fail under sandbox restrictions (`listen tcp 127.0.0.1:0: socket: operation not permitted`).
