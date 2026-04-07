{
  pkgs,
  self',
  inputs',
  preCommit,
}:
let
  # Rust toolchain for building the FFI library from the sibling
  # codetracer-trace-format repo (requires cargo).
  rust-toolchain =
    with inputs'.fenix.packages;
    with stable;
    combine [
      cargo
      rustc
    ];
in
with pkgs;
mkShell {

  hardeningDisable = [ "all" ];

  packages = [

    go_1_24
    go-tools
    golangci-lint

    wabt
    killall

    rust-toolchain
    pkg-config
    capnproto
    delve
    emscripten
    binaryen
    llvm
    just

    figlet
  ]
  ++ preCommit.enabledPackages;

  shellHook = ''
    export EM_CACHE=/tmp/emcc/

    figlet "Welcome to Codetracer WASM recorder!"

    # Detect sibling codetracer-trace-format repo and set up CGO environment
    # for the Rust FFI trace writer. If the sibling is not found, only the
    # pure-Go writer will be available (CGO_ENABLED stays at 0).
    source scripts/detect-trace-format.sh

    ${preCommit.shellHook}
  '';
}
