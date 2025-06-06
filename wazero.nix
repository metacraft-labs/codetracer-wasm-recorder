{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-bratRmw5Leuxx+vWg0bD34/0tley3UX+VKnBPKgHKso=";
}
