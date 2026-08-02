# Code Insights

- Dev shell remains pinned to `pkgs.go_1_24` to avoid Go 1.25 regressions until upstream fixes land; update the roadmap when the toolchain can move forward.
- `DecodeModule` skips DWARF indexing when `dwarf.New` returns nil, preventing crashes on minimal modules.
- DWARF lookups guard missing call-site metadata and tracing helpers skip absent debug info; tracing/DWARF/stylus coverage is still thin and needs focused tests.
- Use `maintester.StripKnownDWARFWarnings` to drop the known DWARF warning before asserting stderr in examples and filecache integration tests.
- The `recorder-golden` wasm fixtures are **compiled by the test run**, not checked in (`cmd/wazero/recorder_golden_fixtures_test.go`). `shell.nix` pins `targets.wasm32-wasip1.stable.rust-std` alongside `stable.rustc` so the compiler is a property of `flake.lock`. That deliberately couples the golden suite's exact line/column/step assertions to the pinned rustc: a toolchain bump can legitimately move them, and `noteGoldenToolchain` prints what happened when it does. Re-review the expectations against the new output — never relax them.
