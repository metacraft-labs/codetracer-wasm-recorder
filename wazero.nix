{ pkgs, ... }:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = [ "cmd/wazero" ];

  vendorHash = "sha256-KPAossSbErDG9cOFxWSYOo+GAsUorevb6YAsvERXo0E=";
}
