{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.vrouter;
  address = if lib.hasInfix ":" cfg.host then "[${cfg.host}]" else cfg.host;
in
{
  options.services.vrouter = {
    enable = lib.mkEnableOption "vrouter model gateway";
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix {}";
      description = "vrouter package, including its embedded frontend.";
    };
    host = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "Listen address. Keep loopback when using an authentication proxy.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "HTTP listen port. The module does not open the firewall.";
    };
    publicUrl = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "https://vrouter.example.com";
      description = "Browser-facing origin for links and management origin checks. Subpaths are unsupported.";
    };
    externalAuth = lib.mkEnableOption ''
      dashboard authentication by a proxy. The proxy must protect every route
      except /v1/*, whose requests use vrouter API keys. Every admitted user is
      an administrator; keep the backend inaccessible to clients
    '';
    windowSkipPlans = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "codex:pro*" ];
      example = [
        "codex:pro*"
        "claude:max 20x"
      ];
      description = ''
        Plans that never get an automatic 5-hour window trigger, as plan or
        provider:plan; a trailing * matches any ending. Use [ "none" ] to
        trigger on every plan.
      '';
    };
    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/run/secrets/vrouter_env";
      description = ''
        Runtime environment file for VROUTER_ADMIN_TOKEN and optional VROUTER_API_KEY.
        Required without externalAuth. Use a runtime path, never a Nix store file.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.externalAuth || cfg.environmentFile != null;
        message = "services.vrouter requires externalAuth or an environmentFile containing VROUTER_ADMIN_TOKEN.";
      }
    ];

    systemd.services.vrouter = {
      description = "vrouter model gateway";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment = {
        VROUTER_ADDR = "${address}:${toString cfg.port}";
        VROUTER_DATA_DIR = "/var/lib/vrouter";
        VROUTER_PUBLIC_URL = cfg.publicUrl;
        VROUTER_EXTERNAL_AUTH = if cfg.externalAuth then "1" else "0";
        VROUTER_WINDOW_SKIP_PLANS = lib.concatStringsSep "," cfg.windowSkipPlans;
      };
      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        DynamicUser = true;
        StateDirectory = "vrouter";
        StateDirectoryMode = "0700";
        WorkingDirectory = "/var/lib/vrouter";
        UMask = "0077";
        Restart = "on-failure";
        RestartSec = 5;
        TimeoutStopSec = 15;
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        RestrictRealtime = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        CapabilityBoundingSet = "";
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
          "~@resources"
        ];
        SystemCallArchitectures = "native";
      };
    };
  };
}
