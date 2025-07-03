{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-zY+MVDZO80VWIhpgnzPklA/glIdO8bD5PlwuSMu502Q=";
}
