{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-wvURkkIWJEzJ8x3RIC9e7jQatSi3uLGXo9O/5+FE/Yo=";

}
