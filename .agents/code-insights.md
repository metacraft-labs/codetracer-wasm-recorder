# Code Insights

- Dev shell remains pinned to `pkgs.go_1_24` to avoid Go 1.25 regressions until upstream fixes land; update the roadmap when the toolchain can move forward.
- `DecodeModule` skips DWARF indexing when `dwarf.New` returns nil, preventing crashes on minimal modules.
- DWARF lookups guard missing call-site metadata and tracing helpers skip absent debug info; tracing/DWARF/stylus coverage is still thin and needs focused tests.
- Use `maintester.StripKnownDWARFWarnings` to drop the known DWARF warning before asserting stderr in examples and filecache integration tests.
- Stylus `emit_log` host hook now resolves ABI signatures and decodes topics/payloads locally (including arrays, dynamic bytes/strings), falling back to hash-only output for dynamic indexed params.
- Stylus log decoder now understands tuple parameter types (including nested/dynamic fields) and renders them as `(v0, v1, …)`; extend tests accordingly when adding new ABI shapes.
