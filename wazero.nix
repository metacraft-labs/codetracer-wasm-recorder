{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-VID7U/dNMs1T5ttDn44ba+4UvbB0tAu//Rn4y66PXis=";
}
