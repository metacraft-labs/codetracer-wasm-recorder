{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-srnouw4s1Rxv8fxpLhD3zj5jkrALwP4O96K86XF8J3E=";
}
