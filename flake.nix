{
  description = "CodeTracer WASM Recorder — a fork of wazero with execution tracing";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    flake-parts.url = "github:hercules-ci/flake-parts";
    fenix = {
      url = "github:nix-community/fenix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    pre-commit-hooks.url = "github:cachix/git-hooks.nix";
    # The CTFS trace writer's C FFI (`packages.trace-writer-ffi`), which the
    # cgo writer in `tracewriter/ctfs_writer.go` links.
    codetracer-trace-format-nim.url = "github:metacraft-labs/codetracer-trace-format-nim/agents";
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
          # git-hooks.nix installs `.pre-commit-config.yaml` and git hooks into
          # `git rev-parse --show-toplevel` of the directory the shell is entered
          # from, so `nix develop /path/to/this-repo` run inside another checkout
          # would plant this repository's hooks there. `ownRepoOnly` runs a snippet
          # only when that toplevel is this repository, recognised by a `flake.nix`
          # identical to the one this shell was evaluated from; anything it cannot
          # establish counts as another repository, so it fails safe.
          # tests/test_dev_shell_writes_nothing_elsewhere.sh
          ownRepoOnly = script: ''
            _own_repo_root="$(${pkgs.git}/bin/git rev-parse --show-toplevel 2>/dev/null || true)"
            if [ -n "$_own_repo_root" ] && [ -f "$_own_repo_root/flake.nix" ] \
              && [ "$(${pkgs.coreutils}/bin/sha256sum "$_own_repo_root/flake.nix" | ${pkgs.coreutils}/bin/cut -d' ' -f1)" \
                = "${builtins.hashFile "sha256" ./flake.nix}" ]; then
            ${script}
            fi
            unset _own_repo_root
          '';
        in
        {
          checks.pre-commit-check = preCommit;

          # The packaged binary records: a Stylus replay of the committed
          # fixture writes a `.ct` container.  This is the property a
          # consumer installing `packages.default` relies on, and the one a
          # build that silently fell back to the stub writer loses.
          checks.packaged-wazero-records = pkgs.runCommand "packaged-wazero-records" { } ''
            ${self'.packages.default}/bin/wazero run \
              --out-dir "$TMPDIR/trace" \
              --stylus=${./cmd/wazero/testdata/stylus/entrypoint_trace.json} \
              ${./cmd/wazero/testdata/stylus/entrypoint.wasm}
            if ! ls "$TMPDIR"/trace/*.ct >/dev/null 2>&1; then
              echo "the packaged wazero exited 0 but wrote no .ct container" >&2
              exit 1
            fi
            touch $out
          '';

          devShells.default = import ./shell.nix {
            inherit
              pkgs
              self'
              inputs'
              preCommit
              ownRepoOnly
              ;
          };

          # Default package: the recording-capable wazero, with the CTFS
          # writer linked from the trace-format FFI.
          packages.default = import ./wazero.nix {
            inherit pkgs;
            codetracer-trace-format-nim = inputs'.codetracer-trace-format-nim.packages.trace-writer-ffi;
          };
        };
    };
}
