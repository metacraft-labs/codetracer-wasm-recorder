{
  pkgs,
  codetracer-trace-writer-ffi ? null,
  ...
}:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = null;

  # When the Rust FFI trace writer library is provided, enable cgo and link
  # against it. This makes the --use-rust-writer flag functional at runtime.
  # CGO_ENABLED goes in `env` to avoid conflicts with buildGoModule internals.
  env.CGO_ENABLED = if codetracer-trace-writer-ffi != null then "1" else "0";

  buildInputs = pkgs.lib.optionals (codetracer-trace-writer-ffi != null) [
    codetracer-trace-writer-ffi
  ];

  # Point cgo at the FFI library's include and lib directories.
  preBuild = pkgs.lib.optionalString (codetracer-trace-writer-ffi != null) ''
    export CGO_CFLAGS="-I${codetracer-trace-writer-ffi}/include"
    export CGO_LDFLAGS="-L${codetracer-trace-writer-ffi}/lib -lcodetracer_trace_writer_ffi -ldl -lm -lpthread"
  '';
}
