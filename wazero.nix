{
  pkgs,
  codetracer-trace-format-nim ? null,
  ...
}:
# wazero CodeTracer fork — CTFS-only Go binary.
#
# The recording path lives in `tracewriter/ctfs_writer.go` and links against
# the Nim C FFI from `codetracer-trace-format-nim`
# (`libcodetracer_trace_writer.a` + `include/codetracer_trace_writer.h`).
# Building without the FFI input still produces a runnable binary, but the
# CTFS writer's stub fallback (`tracewriter/ctfs_writer_stub.go`) returns an
# error from ProduceTrace, so recording is non-functional.  Production
# builds always pass `codetracer-trace-format-nim` from the workspace flake.
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = null;

  # cgo is required to link against the Nim FFI; the non-cgo build is only
  # useful for sandboxed `go vet` runs and falls back to the stub writer.
  env.CGO_ENABLED = if codetracer-trace-format-nim != null then "1" else "0";

  buildInputs = pkgs.lib.optionals (codetracer-trace-format-nim != null) [
    codetracer-trace-format-nim
    pkgs.zstd
  ];

  # Point cgo at the Nim FFI's include and lib directories.  The Nim
  # `buildStaticLib` task drops `libcodetracer_trace_writer.a` next to the source
  # tree, so the upstream package should expose `${out}/lib` and
  # `${out}/include` mirroring those locations.
  preBuild = pkgs.lib.optionalString (codetracer-trace-format-nim != null) ''
    export CGO_CFLAGS="-I${codetracer-trace-format-nim}/include"
    export CGO_LDFLAGS="-L${codetracer-trace-format-nim}/lib -L${pkgs.zstd.out}/lib -Wl,-rpath,${pkgs.zstd.out}/lib"
  '';
}
