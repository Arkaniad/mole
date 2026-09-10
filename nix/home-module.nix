# home-manager module. Runs the daemon as a user service — systemd on Linux,
# a launchd agent on Darwin — and writes the non-secret half of config.json.
{ version }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.programs.mole;
  inherit (lib) mkIf mkMerge optionals;

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

  serveCmd = lib.escapeShellArgs ([ "${cfg.package}/bin/mole" ] ++ serveArgs);

  # launchd has no EnvironmentFile, so the agent sources one in a shell. On
  # systemd the file is read by the manager and never enters a command line.
  darwinScript = pkgs.writeShellScript "mole-serve" ''
    set -eu
    ${lib.optionalString (cfg.environmentFile != null) ''
      set -a
      . ${lib.escapeShellArg (toString cfg.environmentFile)}
      set +a
    ''}
    exec ${serveCmd}
  '';
in
{
  options.programs.mole = import ./options.nix { inherit lib defaultPackage; };

  config = mkIf cfg.enable (mkMerge [
    {
      home.packages = [ cfg.package ];

      # A read-only rendering of what an MCP client needs, so a Claude Code or
      # Codex config can reference one source of truth instead of hardcoding a
      # store path that changes on every update.
      programs.mole.mcpServers = {
        mole = {
          command = "${cfg.package}/bin/mole-mcp";
          args = optionals (cfg.socket != null) [
            "--socket"
            cfg.socket
          ];
        };
      };
    }

    (mkIf (cfg.settings != null) {
      xdg.configFile."mole/config.json".source =
        (pkgs.formats.json { }).generate "mole-config.json"
          cfg.settings;
    })

    (mkIf (cfg.serve.enable && pkgs.stdenv.hostPlatform.isLinux) {
      systemd.user.services.mole = {
        Unit = {
          Description = "mole research daemon";
          Documentation = [ "https://github.com/lajosdeme/mole" ];
          After = [ "network-online.target" ];
          Wants = [ "network-online.target" ];
          # An unconfigured mole exits immediately; without a limit that is a
          # restart loop for the life of the session.
          StartLimitIntervalSec = 60;
          StartLimitBurst = 3;
        };
        Service = {
          ExecStart = serveCmd;
          Restart = "on-failure";
          RestartSec = 5;
          EnvironmentFile = mkIf (cfg.environmentFile != null) (toString cfg.environmentFile);
          # Only the sandboxing a *user* manager can actually apply. The
          # namespace-based options (PrivateTmp, ProtectKernel*) need
          # privileges a user unit does not have and would fail the start
          # rather than harden it; the socket's 0600 mode and the daemon's own
          # peer check are what protect it.
          NoNewPrivileges = true;
          RestrictSUIDSGID = true;
        };
        Install.WantedBy = [ "default.target" ];
      };
    })

    (mkIf (cfg.serve.enable && pkgs.stdenv.hostPlatform.isDarwin) {
      launchd.agents.mole = {
        enable = true;
        config = {
          ProgramArguments = [ "${darwinScript}" ];
          RunAtLoad = true;
          KeepAlive.SuccessfulExit = false;
          # Directly in Library/Logs, which exists: launchd does not create
          # a missing parent directory for these, it just fails to log.
          StandardOutPath = "${config.home.homeDirectory}/Library/Logs/mole.log";
          StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/mole.log";
        };
      };
    })
  ]);
}
