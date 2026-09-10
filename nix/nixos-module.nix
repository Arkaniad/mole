# NixOS module. mole is a single-user tool — its database, config and socket
# all live under one user's XDG directories — so this declares a *user*
# service enabled for the users you name, not a system daemon.
#
# If you already manage the user's environment with home-manager, prefer
# `homeModules.default`; this module exists for hosts that do not.
{ version }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.mole;
  inherit (lib)
    mkIf
    mkOption
    optionals
    types
    ;

  # The overlay's attribute if it is applied, otherwise built here against
  # the same nixpkgs as the rest of this configuration.
  defaultPackage = pkgs.mole-research or (pkgs.callPackage ./package.nix { inherit version; });

  serveArgs = [
    "serve"
  ]
  ++ optionals (cfg.socket != null) [
    "--socket"
    cfg.socket
  ]
  ++ optionals cfg.serve.toolkit [ "--toolkit" ]
  ++ optionals (cfg.serve.maxSessions != null) [
    "--max-sessions"
    (toString cfg.serve.maxSessions)
  ]
  ++ optionals (cfg.serve.workers != null) [
    "--workers"
    (toString cfg.serve.workers)
  ]
  ++ cfg.serve.extraArgs;

  shared = import ./options.nix { inherit lib defaultPackage; };
in
{
  # `settings` is dropped: config.json belongs to a home directory, and NixOS
  # has no clean way to write into one. Use the home-manager module for it, or
  # `mole config set`.
  options.services.mole = (removeAttrs shared [ "settings" ]) // {
    users = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "alice" ];
      description = ''
        Restrict the user service to these users, and keep their session alive
        across logout. Empty means every user session runs it — fine on a
        single-user machine, wasteful on a shared one.
      '';
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    services.mole.mcpServers = {
      mole = {
        command = "${cfg.package}/bin/mole-mcp";
        args = optionals (cfg.socket != null) [
          "--socket"
          cfg.socket
        ];
      };
    };

    systemd.user.services.mole = {
      description = "mole research daemon";
      documentation = [ "https://github.com/lajosdeme/mole" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = optionals cfg.serve.enable [ "default.target" ];
      # Conditions of the same type are ORed, so a non-empty list is "these
      # users and no others"; every other user's session skips the unit
      # instead of failing it.
      unitConfig = {
        # An unconfigured mole exits immediately; without a limit that is a
        # restart loop for the life of the session.
        StartLimitIntervalSec = 60;
        StartLimitBurst = 3;
      }
      // lib.optionalAttrs (cfg.users != [ ]) { ConditionUser = cfg.users; };
      serviceConfig = {
        ExecStart = lib.escapeShellArgs ([ "${cfg.package}/bin/mole" ] ++ serveArgs);
        Restart = "on-failure";
        RestartSec = 5;
        EnvironmentFile = mkIf (cfg.environmentFile != null) (toString cfg.environmentFile);
        # Only the sandboxing a *user* manager can actually apply; the
        # namespace-based options need privileges a user unit does not have.
        NoNewPrivileges = true;
        RestrictSUIDSGID = true;
      };
    };

    # Without lingering the daemon dies at logout, which defeats the point of
    # research that outlives an editor session.
    users.users = lib.genAttrs cfg.users (_: {
      linger = true;
    });
  };
}
