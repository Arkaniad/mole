# Options shared by the NixOS and home-manager modules.
#
# Both modules run the same daemon for the same user; only the mechanism for
# declaring the unit and the config file differs. Keeping the option surface in
# one place means `programs.mole.settings` means the same thing wherever it is
# set.
{ lib, defaultPackage }:

let
  inherit (lib) mkOption mkEnableOption types;
in
{
  enable = mkEnableOption "mole, a deep research agent exposed over MCP";

  package = mkOption {
    type = types.package;
    default = defaultPackage;
    defaultText = lib.literalMD "`pkgs.mole-research`, or this flake's package built against your nixpkgs";
    description = "The mole package providing `mole` and `mole-mcp`.";
  };

  socket = mkOption {
    type = types.nullOr types.str;
    default = null;
    example = "/run/user/1000/mole.sock";
    description = ''
      Unix socket the daemon listens on and `mole-mcp` connects to. `null`
      takes mole's own default: `$XDG_RUNTIME_DIR/mole.sock`, falling back to
      the state directory where that is unset (Darwin, bare containers).

      Set here rather than in {option}`serve.extraArgs` so that
      {option}`mcpServers` stays in agreement with the running daemon.
    '';
  };

  serve = {
    enable = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Run `mole serve` as a per-user service.

        Research outlives an editor session, which is the whole reason the
        daemon exists: `mole-mcp` is spawned and killed by the editor while the
        work continues here. Turn this off only if you intend to start the
        daemon yourself.
      '';
    };

    maxSessions = mkOption {
      type = types.nullOr types.ints.positive;
      default = null;
      description = "Value for `--max-sessions`; null leaves the daemon default.";
    };

    workers = mkOption {
      type = types.nullOr types.ints.positive;
      default = null;
      description = "Value for `--workers`; null leaves the daemon default.";
    };

    toolkit = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Pass `--toolkit`, exposing the individual research tools over MCP
        instead of only the end-to-end research surface.
      '';
    };

    extraArgs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [
        "--shutdown-grace"
        "2m"
      ];
      description = "Extra arguments appended to `mole serve`.";
    };
  };

  environmentFile = mkOption {
    type = types.nullOr types.path;
    default = null;
    example = "/run/agenix/mole-env";
    description = ''
      Path to a file of `KEY=value` lines loaded into the daemon's environment.
      This is where credentials belong: environment values override the config
      file, so nothing secret has to enter the Nix store.

      Recognised keys include `MOLE_LLM_API_KEY` (or `ANTHROPIC_API_KEY`),
      `MOLE_BRAVE_API_KEY`, `MOLE_TAVILY_API_KEY`, `MOLE_SEARXNG_URL`,
      `MOLE_SEARXNG_TOKEN` and `MOLE_CONTACT_EMAIL`.

      The file is read at runtime by the service manager, so use a path
      produced by agenix/sops-nix rather than a literal path in this repo — a
      bare path expression would be copied to the store.
    '';
  };

  settings = mkOption {
    type = types.nullOr (types.attrsOf types.anything);
    default = null;
    example = lib.literalExpression ''
      {
        search.provider = "searxng";
        search.searxng_url = "http://localhost:8888";
        llm = {
          provider = "anthropic";
          model = "claude-sonnet-5";
          cheap_model = "claude-haiku-4-5-20251001";
        };
        contact_email = "you@example.com";
        max_session_usd = 2000000; # micro-dollars: $2.00
      }
    '';
    description = ''
      Contents of `$XDG_CONFIG_HOME/mole/config.json`, written declaratively.
      `null` (the default) leaves the file alone, so `mole config set` keeps
      working.

      Do not put API keys here. This file lands in the world-readable Nix
      store; use {option}`environmentFile` for anything secret.
    '';
  };

  mcpServers = mkOption {
    type = types.attrsOf (types.attrsOf types.anything);
    readOnly = true;
    description = ''
      Read-only `mcpServers` fragment for an MCP client's configuration,
      pointing at this package's `mole-mcp` and this module's socket. Merge it
      into whatever writes your client config so a package update does not
      leave a stale store path behind:

      ```nix
      home.file.".mcp.json".text = builtins.toJSON {
        mcpServers = config.programs.mole.mcpServers;
      };
      ```
    '';
  };
}
