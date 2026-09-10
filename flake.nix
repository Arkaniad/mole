{
  description = "mole — a deep research agent in Go, exposed over MCP";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # A binary should be able to say which source built it. The tag is the
      # authority; the flake appends the revision so a build from an untagged
      # commit is not silently reported as the tag.
      baseVersion = "0.1.0";
      version =
        baseVersion
        +
          nixpkgs.lib.optionalString (self ? shortRev || self ? dirtyShortRev)
            "+${self.shortRev or self.dirtyShortRev}";
    in
    {
      # `mole-research`, not `mole`: nixpkgs already has a `mole` (an SSH
      # tunnelling tool), and an overlay that silently replaced it would be a
      # trap. Same resolution the AUR packaging in this repo uses.
      overlays.default = final: _prev: {
        mole-research = final.callPackage ./nix/package.nix { inherit version; };
      };

      packages = forAllSystems (pkgs: rec {
        mole-research = pkgs.callPackage ./nix/package.nix { inherit version; };
        default = mole-research;
      });

      apps = forAllSystems (pkgs: {
        # `nix run github:lajosdeme/mole` — the CLI.
        default = self.apps.${pkgs.stdenv.hostPlatform.system}.mole;
        mole = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.default}/bin/mole";
        };
        # `nix run github:lajosdeme/mole#mole-mcp` — usable directly as an MCP
        # command, at the cost of an evaluation on every editor spawn.
        mole-mcp = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.default}/bin/mole-mcp";
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            go-tools # staticcheck
            golangci-lint
            goreleaser
            sqlite # inspecting the session database by hand
          ];
          env.CGO_ENABLED = "0";
        };
      });

      checks = forAllSystems (pkgs: {
        # The package's own checkPhase runs `go test ./...`; building it is the
        # check. A second, separately-derived test run would only be a slower
        # way to compile the same code twice.
        mole = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);

      # The modules build against the *user's* nixpkgs rather than this
      # flake's, so a config that already has one does not pull in a second.
      nixosModules.default = import ./nix/nixos-module.nix { inherit version; };
      homeModules.default = import ./nix/home-module.nix { inherit version; };
      # home-manager renamed this output; keep the old name resolvable.
      homeManagerModules.default = self.homeModules.default;
    };
}
