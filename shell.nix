{
  pkgs,
  self',
  inputs',
  preCommit,
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

    go_1_24
    go-tools
    golangci-lint

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
  ] ++ preCommit.enabledPackages;

  shellHook = ''
    export EM_CACHE=/tmp/emcc/

    figlet "Welcome to Codetracer WASM recorder!"
    ${preCommit.shellHook}
  '';
}
