{
  pkgs,
  self',
  inputs',
  preCommit,
}:
let
  # Rust toolchain for building the FFI library from the sibling
  # codetracer-trace-format repo (requires cargo), and for compiling
  # the `cmd/wazero/testdata/recorder-golden/*.rs` fixtures to
  # wasm32-wasip1 during `go test`.
  #
  # `wasm32-wasip1` rust-std comes from the same pinned fenix `stable`
  # channel as `rustc` itself, so the fixture compiler — and therefore
  # the DWARF the recorder's stepping assertions read — is a property
  # of `flake.lock`, not of whatever rustup happened to be on the
  # builder's PATH.  Bumping the fenix input moves both together.
  rust-toolchain =
    with inputs'.fenix.packages;
    combine [
      stable.cargo
      stable.rustc
      targets.wasm32-wasip1.stable.rust-std
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
    prek

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
