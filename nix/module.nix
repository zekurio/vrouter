{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.vrouter;
  address = if lib.hasInfix ":" cfg.host then "[${cfg.host}]" else cfg.host;
  groups = lib.concatStringsSep "," (
    lib.mapAttrsToList (group: role: "${group}=${role}") cfg.oidc.groups
  );
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
      description = "Listen address. Keep loopback behind a reverse proxy.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "HTTP listen port. The module does not open the firewall.";
    };
    publicUrl = lib.mkOption {
      type = lib.types.str;
      example = "https://vrouter.example.com";
      description = ''
        Browser-facing origin for sign-in redirects, links, and management
        origin checks. Subpaths are unsupported. The OIDC client's callback URL
        is this origin followed by /api/auth/callback.
      '';
    };
    oidc = {
      issuer = lib.mkOption {
        type = lib.types.str;
        example = "https://id.example.com";
        description = "Issuer URL of the OpenID Connect provider, such as Pocket ID.";
      };
      clientId = lib.mkOption {
        type = lib.types.str;
        description = "Client ID of the OIDC client registered for vrouter.";
      };
      clientSecretFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/run/secrets/vrouter_oidc_secret";
        description = ''
          File holding the OIDC client secret. systemd passes it to the service
          as a credential, so it can stay readable by root alone. Leave it null
          for a public client. Use a runtime path, never a Nix store file.
        '';
      };
      groups = lib.mkOption {
        type = lib.types.attrsOf (
          lib.types.enum [
            "admin"
            "member"
          ]
        );
        example = {
          vrouter_admins = "admin";
          vrouter_users = "member";
        };
        description = ''
          Provider groups and the vrouter role each one grants. People in no
          listed group cannot sign in. At least one group must grant admin.
        '';
      };
    };
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
        Optional runtime environment file for other VROUTER_* variables.
        Use a runtime path, never a Nix store file.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = lib.elem "admin" (lib.attrValues cfg.oidc.groups);
        message = "services.vrouter.oidc.groups must grant admin to at least one group.";
      }
      {
        assertion = lib.all (group: !(lib.hasInfix "," group || lib.hasInfix "=" group)) (
          lib.attrNames cfg.oidc.groups
        );
        message = "services.vrouter.oidc.groups names cannot contain commas or equals signs.";
      }
    ];

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
        VROUTER_PUBLIC_URL = cfg.publicUrl;
        VROUTER_OIDC_ISSUER = cfg.oidc.issuer;
        VROUTER_OIDC_CLIENT_ID = cfg.oidc.clientId;
        VROUTER_OIDC_GROUPS = groups;
        VROUTER_WINDOW_SKIP_PLANS = lib.concatStringsSep "," cfg.windowSkipPlans;
      }
      // lib.optionalAttrs (cfg.oidc.clientSecretFile != null) {
        # systemd expands %d to the service's credentials directory.
        VROUTER_OIDC_CLIENT_SECRET_FILE = "%d/oidc-client-secret";
      };
      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        LoadCredential = lib.optional (
          cfg.oidc.clientSecretFile != null
        ) "oidc-client-secret:${cfg.oidc.clientSecretFile}";
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
