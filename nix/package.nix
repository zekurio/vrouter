{
  lib,
  buildGo126Module,
  stdenvNoCC,
  nodejs_24,
  pnpm,
  pnpmConfigHook,
  fetchPnpmDeps,
}:
let
  frontend = stdenvNoCC.mkDerivation (finalAttrs: {
    pname = "vrouter-ui";
    version = "0.1.0";
    src = lib.fileset.toSource {
      root = ../web;
      fileset = lib.fileset.unions [
        ../web/src
        ../web/public
        ../web/index.html
        ../web/package.json
        ../web/pnpm-lock.yaml
        ../web/tsconfig.json
        ../web/vite.config.ts
      ];
    };
    nativeBuildInputs = [
      nodejs_24
      pnpm
      pnpmConfigHook
    ];
    pnpmDeps = fetchPnpmDeps {
      inherit (finalAttrs) pname version src;
      inherit pnpm;
      fetcherVersion = 4;
      # pnpm 12.7 stopped fetching other platforms with --force by default.
      # The fixed-output store must be the same on Linux and macOS.
      prePnpmInstall = ''
        export pnpm_config_force_ignores_platform=true
      '';
      hash = "sha256-hcX9WY8iBhipFr9s32w0mFx4JLalXt6FraOJACx3LOI=";
    };
    buildPhase = ''
      runHook preBuild
      pnpm build
      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      cp -r dist $out
      runHook postInstall
    '';
  });
in
buildGo126Module {
  pname = "vrouter";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      ../internal
      ../go.mod
      ../web/embed.go
    ];
  };
  vendorHash = null;
  subPackages = [ "cmd/vrouter" ];
  env.CGO_ENABLED = 0;
  preBuild = ''
    cp -r ${frontend} web/dist
  '';
  # Test the gateway package too, not just the command selected for installation.
  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';
  ldflags = [
    "-s"
    "-w"
  ];
  passthru = { inherit frontend; };
  meta = {
    description = "Model gateway with an embedded management UI";
    mainProgram = "vrouter";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
      "aarch64-darwin"
    ];
  };
}
