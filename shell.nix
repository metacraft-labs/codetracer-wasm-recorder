{
  pkgs,
  self',
  inputs',
}:
let
  wasm-rust =
    with inputs'.fenix.packages;
    with latest;
    combine [
      cargo
      rustc
      llvm-tools
      targets.wasm32-unknown-unknown.latest.rust-std
    ];
in
with pkgs;
mkShell {

  hardeningDisable = [ "all" ];

  packages = [

    go
    wabt
    killall

    # cargo
    # wasm-rust
    delve
    emscripten
    binaryen
    llvm
    just

    figlet
  ];

  shellHook = ''
    export EM_CACHE=/tmp/emcc/

    figlet "welcome to wasmi recorder"
  '';
}
