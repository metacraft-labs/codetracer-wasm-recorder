{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-qo9oC0E39jPJlcM9POoARcqDKJlRWEl0jWTlflsFFQQ=";

}
