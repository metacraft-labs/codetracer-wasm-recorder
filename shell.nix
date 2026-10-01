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
    # `just lint-nix`. From the pinned nixpkgs, so the check does not depend
    # on whether (and which) nixfmt the host happens to have.
    nixfmt

    figlet
  ]
  ++ preCommit.enabledPackages;

  # `cargo <subcommand>` looks for `cargo-<subcommand>` in `$CARGO_HOME/bin`
  # BEFORE it searches PATH. On any machine with rustup — including the
  # self-hosted macOS runner — that directory holds rustup's proxies, so a
  # cargo subcommand runs rustup's toolchain instead of the one above, and
  # fails with "'cargo-<subcommand>' is not installed for the toolchain".
  #
  # The shell therefore gets its own CARGO_HOME with an empty `bin/`, so
  # subcommand lookup falls through to PATH. `registry/` and `git/` are
  # symlinks to the real CARGO_HOME, and so are its config and credentials
  # when present: the download cache is shared, and only the proxy directory
  # is left behind.
  shellHook = ''
    _wasm_real_cargo_home="''${CARGO_HOME:-$HOME/.cargo}"
    _wasm_cargo_home="''${XDG_CACHE_HOME:-$HOME/.cache}/codetracer-wasm-recorder/cargo-home"
    if [ "$_wasm_real_cargo_home" != "$_wasm_cargo_home" ]; then
      mkdir -p "$_wasm_cargo_home" \
        "$_wasm_real_cargo_home/registry" "$_wasm_real_cargo_home/git"
      # Re-pointed on every entry, so a changed CARGO_HOME is followed
      # rather than left sharing the previous one's cache. Only a link
      # is ever replaced; a real file placed here is left alone.
      for _wasm_entry in registry git config.toml credentials.toml; do
        if [ -e "$_wasm_real_cargo_home/$_wasm_entry" ] &&
          { [ -L "$_wasm_cargo_home/$_wasm_entry" ] ||
            [ ! -e "$_wasm_cargo_home/$_wasm_entry" ]; }; then
          ln -sfn "$_wasm_real_cargo_home/$_wasm_entry" "$_wasm_cargo_home/$_wasm_entry"
        fi
      done
      export CARGO_HOME="$_wasm_cargo_home"
    fi
    unset _wasm_real_cargo_home _wasm_cargo_home _wasm_entry

    export EM_CACHE=/tmp/emcc/

    figlet "Welcome to Codetracer WASM recorder!"

    # Detect sibling codetracer-trace-format repo and set up CGO environment
    # for the Rust FFI trace writer. If the sibling is not found, only the
    # pure-Go writer will be available (CGO_ENABLED stays at 0).
    source scripts/detect-trace-format.sh

    ${preCommit.shellHook}
  '';
}
