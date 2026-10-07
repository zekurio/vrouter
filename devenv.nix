{ pkgs, ... }:

{
  packages = with pkgs; [
    go_1_26
    gopls
    nodejs_24
    pnpm
    just
    librsvg
  ];
}
