# The mole package: both binaries from one derivation.
#
# mole-mcp is a shim that forwards to the daemon mole provides, so splitting
# them into separate outputs would only let someone install half a working
# system. goreleaser ships them in one archive for the same reason.
{
  lib,
  buildGoModule,
  installShellFiles,
  stdenv,
  # Version reported by `mole version`. The flake passes the tag plus the
  # revision so a store path and the binary inside it cannot disagree.
  version ? "0.1.0",
}:

buildGoModule (finalAttrs: {
  # Not "mole": nixpkgs already ships an unrelated SSH tunnelling tool under
  # that name, and the AUR packaging in this repo settled the same collision
  # the same way. The binaries are still `mole` and `mole-mcp`.
  pname = "mole-research";
  inherit version;

  # Only what the build reads. The two banner PNGs are ~2MB each and would
  # otherwise land in the store and invalidate the build on every image tweak.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ../testdata
      ../LICENSE
      ../NOTICE
    ];
  };

  vendorHash = "sha256-z/wZZlTVXlhwlCz7un9WN+CMSg1F/2O6HSvLRtrqu1g=";

  # Everything but the probe, a development aid with no place in a profile.
  # Not `subPackages`, which would also narrow the test phase to those two
  # directories and skip every test under internal/.
  excludedPackages = [ "cmd/mcp-probe" ];

  # The pure-Go SQLite driver is the reason this is possible: one static
  # binary, no libc to match at runtime.
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}"
  ];

  # Matches .goreleaser.yaml, and keeps build paths out of the binary.
  tags = [ ];
  flags = [ "-trimpath" ];

  # `go test ./...` in the sandbox: the suite stands up its own httptest
  # servers and temp dirs, so a build failing here is a real failure.
  doCheck = true;

  nativeBuildInputs = [ installShellFiles ];

  postInstall = lib.optionalString (stdenv.buildPlatform.canExecute stdenv.hostPlatform) ''
    installShellCompletion --cmd mole \
      --bash <($out/bin/mole completion bash) \
      --fish <($out/bin/mole completion fish) \
      --zsh  <($out/bin/mole completion zsh)
  '';

  meta = {
    description = "Deep research agent in Go, exposed over MCP";
    longDescription = ''
      mole runs multi-step research — search, fetch, extract, verify — under an
      explicit token or dollar budget, and exposes it over MCP so a coding agent
      can drive it. The daemon (`mole serve`) holds session and budget state;
      `mole-mcp` is a disposable stdio shim an editor spawns and kills.
    '';
    homepage = "https://github.com/lajosdeme/mole";
    license = lib.licenses.asl20;
    mainProgram = "mole";
    platforms = lib.platforms.unix;
  };
})
