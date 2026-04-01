{
  description = "CodeTracer WASM Recorder — a fork of wazero with execution tracing";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    fenix = {
      url = "github:nix-community/fenix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    pre-commit-hooks.url = "github:cachix/git-hooks.nix";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      perSystem =
        {
          pkgs,
          inputs',
          self',
          system,
          ...
        }:
        let
          preCommit = inputs.pre-commit-hooks.lib.${system}.run {
            src = ./.;
            hooks = {
              lint = {
                enable = true;
                name = "Lint";
                entry = "just lint";
                language = "system";
                pass_filenames = false;
              };
            };
          };
        in
        {
          checks.pre-commit-check = preCommit;

          devShells.default = import ./shell.nix {
            inherit
              pkgs
              self'
              inputs'
              preCommit
              ;
          };

          # Default package: wazero without FFI (zero dependencies, pure Go).
          # To build with the Rust FFI trace writer, the consuming flake (e.g.
          # codetracer) should pass a pre-built codetracer-trace-writer-ffi
          # package from codetracer-trace-format to wazero.nix.
          packages.default = import ./wazero.nix { inherit pkgs; };
        };
    };
}
