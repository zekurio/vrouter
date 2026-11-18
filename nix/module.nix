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
  imports = [
    (lib.mkRemovedOptionModule [ "services" "vrouter" "oidc" ] ''
      vrouter no longer signs people in through OpenID Connect. Set
      services.vrouter.adminKeyFile instead.
    '')
  ];

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
      description = "Listen address. Keep loopback behind a reverse proxy.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "HTTP listen port. The module does not open the firewall.";
    };
    publicUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "https://vrouter.example.com";
      description = ''
        Browser-facing origin for links, provider sign-in, and management
        origin checks. Subpaths are unsupported. Set it whenever vrouter sits
        behind a reverse proxy.
      '';
    };
    adminKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/run/secrets/vrouter_admin_key";
      description = ''
        File holding the admin key, which opens the dashboard and the
        management API. It must be at least 16 characters. systemd passes it
        to the service as a credential, so it can stay readable by root alone.
        Use a runtime path, never a Nix store file. With null, vrouter makes a
        new key at every start and prints it to the journal.
      '';
    };
    windowSkipPlans = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "codex:pro*" ];
      example = [
        "codex:pro*"
        "claude:ultra"
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
        Optional runtime environment file for other VROUTER_* variables.
        Use a runtime path, never a Nix store file.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.vrouter = {
      description = "vrouter model gateway";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment = {
        VROUTER_ADDR = "${address}:${toString cfg.port}";
        # systemd resolves WorkingDirectory before launch. With DynamicUser,
        # /var/lib/vrouter can be a symlink, which the data store refuses.
        VROUTER_DATA_DIR = ".";
        VROUTER_WINDOW_SKIP_PLANS = lib.concatStringsSep "," cfg.windowSkipPlans;
      }
      // lib.optionalAttrs (cfg.publicUrl != null) {
        VROUTER_PUBLIC_URL = cfg.publicUrl;
      }
      // lib.optionalAttrs (cfg.adminKeyFile != null) {
        # systemd expands %d to the service's credentials directory.
        VROUTER_ADMIN_KEY_FILE = "%d/admin-key";
      };
      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        LoadCredential = lib.optional (cfg.adminKeyFile != null) "admin-key:${cfg.adminKeyFile}";
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
