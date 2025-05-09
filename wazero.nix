{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-hkm1U27PFra68niY+apw1vc/mA2znz+EjHvmGhJ3XcY=";

}
