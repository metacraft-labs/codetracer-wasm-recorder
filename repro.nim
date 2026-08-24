## Reprobuild dev env + build recipe for codetracer-wasm-recorder.
##
## codetracer-wasm-recorder is a Go module (a fork of
## ``github.com/tetratelabs/wazero``) that ships the ``wazero`` CLI with
## CodeTracer execution tracing. ``repro build`` / ``repro test``
## reproduce the same primary artefact (the ``wazero`` binary) and the
## same pure-Go writer test entry point that ``just build`` /
## ``just test-tracewriter-go`` produce today, so CI can cross-check the
## reprobuild path against the legacy Nix/Just path per
## ``codetracer-specs/Repo-Requirements.md`` §2.9.
##
## ## Scoping decision — pure-Go (``CGO_ENABLED=0``) only
##
## The recorder's on-disk CTFS writer is implemented **twice**, gated by
## the ``cgo`` build tag:
##
##   * ``tracewriter/ctfs_writer.go`` is ``//go:build cgo`` and links the
##     Rust FFI library ``codetracer_trace_writer_ffi`` (``-lcodetracer_
##     trace_writer -lzstd -lm -lpthread``) exposed by the sibling
##     ``codetracer-trace-format-nim`` repo. It carries an ``import "C"``
##     block.
##   * ``tracewriter/ctfs_writer_stub.go`` is ``//go:build !cgo`` and is
##     the pure-Go fallback — it needs no external C/Rust library and
##     returns a loud error if a recording is actually requested.
##
## This recipe deliberately targets the **pure-Go build**
## (``CGO_ENABLED=0``, which selects the ``!cgo`` stub). It does NOT
## build the Rust FFI library nor exercise the cgo recording path. That
## path stays covered by the existing ``.github/workflows/ci.yml`` job,
## which builds ``codetracer_trace_writer_ffi`` with cargo and then runs
## ``CGO_ENABLED=1 go test ./tracewriter/`` against it. Reprobuild has no
## business reproducing the cross-language FFI link here — the cgo
## convention would need a C cross-toolchain plus the sibling Rust crate,
## which is exactly what ci.yml already owns.
##
## Because ``import "C"`` appears anywhere in the module (both
## ``ctfs_writer.go`` here and the upstream fuzz harness
## ``internal/integration_test/fuzz/wazerolib/extern.go``), the standard
## provider's fine-grained Go convention (Mode A —
## ``repro_standard_provider/conventions/go.nim``) declines to recognise
## the project: its ``recognize`` returns ``false`` the moment it sees a
## cgo trigger, forcing a Mode-B ("crude") wrap. We therefore take the
## coarse option-A approach the sibling recipes and ``io-mon`` take:
## wrap the repo's *existing* build/test entry points verbatim with
## ``sh.shell`` execute edges rather than re-deriving a per-package Go
## action graph. Each wrapped command pins ``CGO_ENABLED=0`` so the
## engine-driven build is byte-for-byte the pure-Go build the Justfile's
## ``build`` / ``test-tracewriter-go`` recipes produce.
##
## ## Exposed edges
##
##   * ``repro build`` (the ``default`` collection) — ``CGO_ENABLED=0 go
##     build -o wazero ./cmd/wazero``, the same command as ``just build``
##     but with cgo pinned off. Produces the ``wazero`` CLI.
##   * ``repro test`` (the ``test`` collection) — ``CGO_ENABLED=0 go test
##     ./tracewriter/ -v -run 'TestGoWriter'``, one-for-one with the
##     Justfile's ``test-tracewriter-go`` recipe, the repo's canonical
##     pure-Go writer test entry point (kept stable so external CI does
##     not break — see the Justfile comment). The cgo-dependent recording
##     tests (``cmd/wazero`` recording tests, the cgo ``tracewriter``
##     writer) are intentionally NOT run here: they require the FFI
##     library and are gated by ci.yml.
##
## ## Vendored dependencies — offline build
##
## The module vendors its single external dependency
## (``github.com/rdleal/intervalst``) under ``vendor/``, so ``go build`` /
## ``go test`` resolve it in ``-mod=vendor`` mode (Go's default when
## ``vendor/modules.txt`` is present) without any network access.
##
## ## Tool provisioning
##
## ``defaultToolProvisioning "path"`` matches the canonical recorder
## recipes: the Nix dev shell (``flake.nix`` → ``shell.nix``) puts ``go``
## on ``PATH`` on Linux/macOS; on Windows the ``go`` package's tarball
## catalog entry provisions it. Either way the weak-local PATH resolver
## is the right default. ``sh`` drives the two ``shell(...)`` edges.

import repro_project_dsl
import repro_dsl_stdlib/packages/sh

package codetracer_wasm_recorder:
  defaultToolProvisioning "path"

  uses:
    # Go toolchain floor — mirrors ``go.mod``'s ``go 1.22.0`` directive.
    # On Linux/macOS the nix dev shell supplies ``go_1_24``; on Windows
    # the ``go`` package's versioned tarball catalog provisions it.
    "go >=1.22"
    # POSIX shell — runs the two wrapped ``go`` commands below (it is the
    # tool every ``shell(...)`` edge invokes). ``CGO_ENABLED=0`` is set
    # inline in each command string.
    "sh"

    # Chocolatey — `choco pack` / `choco push` in this repo's
    # .github/workflows/publish-chocolatey.yml. Declared here rather than
    # installed on the runner: a tool this repo needs is this repo's
    # dependency, and leaving it to the machine means a developer and CI each
    # get whatever their box happens to carry.
    #
    # BOTH halves below are load-bearing and they answer different questions.
    # `platforms: [windows]` on the package (reprobuild's catalog) says where
    # chocolatey CAN exist; this guard says whether THIS recipe needs it here.
    # No package-side declaration can answer the second — which is why Nix
    # keeps meta.platforms beside lib.optionals, and Spack requires() beside
    # depends_on(when=). Do not delete the guard on the grounds that the
    # package now declares its platform.
    when defined(windows):
      "chocolatey"

  # The primary shipping artefact. The on-disk binary is ``wazero`` (the
  # one documented exception to the ``codetracer-<lang>-recorder`` naming
  # pattern); a Nim identifier must be a valid ident so it is declared
  # ``wazero`` directly. The actual compile is the explicit ``build:``
  # edge below.
  executable wazero:
    name: "wazero"

  devEnv:
    activity "default"

  build:
    const binSuffix = (when defined(windows): ".exe" else: "")
    const wazeroBinary = "wazero" & binSuffix

    # ---- Primary build edge (the `default` collection) ----------------
    #
    # Wraps ``just build`` (``go build -o wazero ./cmd/wazero``) verbatim
    # with ``CGO_ENABLED=0`` pinned so the pure-Go ``!cgo`` stub is
    # selected and no external C/Rust library is needed. Enrolled into the
    # conventional ``default`` collection per
    # ``reprobuild-specs/Build-Graph-Collections.md`` so a bare
    # ``repro build`` materialises this edge's closure.
    let wazeroBuild = shell(
      command = "CGO_ENABLED=0 go build -o " & wazeroBinary & " ./cmd/wazero",
      actionId = "codetracer-wasm-recorder.go-build",
      extraInputs = @[
        "go.mod",
        "go.sum",
        "vendor",
        "cmd",
        "internal",
        "experimental",
        "imports",
        "api",
        "sys",
        "tracewriter",
        "builder.go",
        "cache.go",
        "config.go",
        "fsconfig.go",
        "runtime.go",
      ],
      extraOutputs = @[wazeroBinary])
    discard collect("default", @[wazeroBuild])

    # ---- Pure-Go writer test edge (the `test` collection) -------------
    #
    # One-for-one with the Justfile's ``test-tracewriter-go`` recipe: the
    # repo's canonical pure-Go writer test entry point. ``CGO_ENABLED=0``
    # selects the ``!cgo`` stub so no FFI library is required. The
    # cgo-dependent recording tests stay in ci.yml (see the header).
    let writerTest = shell(
      command = "CGO_ENABLED=0 go test ./tracewriter/ -v -run 'TestGoWriter'",
      actionId = "codetracer-wasm-recorder.go-test-tracewriter",
      extraInputs = @[
        "go.mod",
        "go.sum",
        "vendor",
        "tracewriter",
        "internal",
      ])
    discard collect("test", @[writerTest])
