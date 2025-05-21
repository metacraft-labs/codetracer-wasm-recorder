{pkgs, ...}:
pkgs.buildGoModule rec {
  name = "wazero";
  pname = name;

  src = ./.;

  doCheck = false;

  subPackages = ["cmd/wazero"];

  vendorHash = "sha256-9zS4ex2GXMaJKvQ/rui1RvbcZ4z7C/u2/7Q5Ls1Qlvk=";
}
