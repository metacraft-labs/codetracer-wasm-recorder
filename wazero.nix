{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-rne635vYt9iSMs3Rcd2E7NE8f/eQvvAiToohfYPDmi0=";

}
