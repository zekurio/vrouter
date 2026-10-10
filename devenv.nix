{ pkgs, ... }:

{
  packages = with pkgs; [
    go_1_26
    gopls
    golangci-lint
    nodejs_24
    pnpm
    just
    librsvg
  ];
}
