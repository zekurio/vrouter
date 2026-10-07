{
  description = "vrouter model gateway and NixOS service";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          vrouter = pkgs.callPackage ./nix/package.nix { };
          default = self.packages.${system}.vrouter;
        }
      );

      nixosModules.vrouter = import ./nix/module.nix;
      nixosModules.default = self.nixosModules.vrouter;

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
      checks = forAllSystems (system: {
        package = self.packages.${system}.vrouter;
      });
    };
}
