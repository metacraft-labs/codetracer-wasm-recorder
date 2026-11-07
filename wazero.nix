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
# `codetracer-trace-format-nim` is that FFI as built by its flake's
# `packages.trace-writer-ffi` (`lib/libcodetracer_trace_writer.a`,
# `include/*.h`).  Without it the binary still runs modules, but the CTFS
# writer's stub (`tracewriter/ctfs_writer_stub.go`) refuses to write a trace
# and every `--out-dir` run exits non-zero.
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

  # Point cgo at the FFI's include and lib directories.  The package ships
  # only the static archive, so the writer is linked into the binary.
  preBuild = pkgs.lib.optionalString (codetracer-trace-format-nim != null) ''
    export CGO_CFLAGS="-I${codetracer-trace-format-nim}/include"
    export CGO_LDFLAGS="-L${codetracer-trace-format-nim}/lib -L${pkgs.zstd.out}/lib -Wl,-rpath,${pkgs.zstd.out}/lib"
  '';
}
