{
  description = "vrouter development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          # gcc is only needed for `go test -race`; release builds use CGO_ENABLED=0.
          packages = with pkgs; [
            go
            gopls
            nodejs_24
            gcc
            gnumake
            librsvg
          ];
        };
      });
    };
}
