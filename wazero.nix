{ pkgs, ... }:
let
  inherit (pkgs) ;
in
buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./cmd/wazero;

  vendorHash = lib.fakeHash;
}
