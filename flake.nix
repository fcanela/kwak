{
  description = "kwak: AI coding sessions talk to each other on a private Matrix server";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      # Only x86_64-linux is built and tested. The others evaluate and should build: kwak is pure
      # Go with CGO off.
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # One version, read from the code, so the package and `kwak version` cannot disagree.
      version = builtins.head (
        builtins.match ".*const Version = \"([^\"]+)\".*" (builtins.readFile ./internal/wire/api.go)
      );

      src = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [ ./go.mod ./go.sum ./cmd ./internal ./plugins ./test ./integrations ./.claude-plugin ];
      };

      build =
        pkgs: pname: extra:
        pkgs.buildGoModule (
          {
            inherit pname version src;
            vendorHash = "sha256-Sap9h85JZr8q1vUtR3B7aFfGQ+LpJ1ZS7/ZolfaDFN4=";
            subPackages = [ "cmd/${pname}" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
            # Every unit test, not only the ones under subPackages. The integration and end-to-end
            # suites need Docker and a build tag, so they stay out.
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';
            meta = {
              homepage = "https://github.com/fcanela/kwak";
              license = lib.licenses.mit;
              mainProgram = pname;
            };
          }
          // extra
        );
    in
    {
      packages = forAll (pkgs: rec {
        # The listener, hooks and tools, plus the Claude Code plugin from the same commit, so the
        # plugin always matches the binary. Load it with --plugin-dir (home-manager:
        # programs.claude-code.plugins) instead of the marketplace, never both.
        kwak = build pkgs "kwak" {
          postInstall = ''
            mkdir -p $out/share/kwak
            cp -r plugins/claude $out/share/kwak/claude-plugin
          '';
        };
        # The server side. It runs next to the Matrix server, not on agent machines.
        kwak-registrar = build pkgs "kwak-registrar" { };
        default = kwak;
      });
    };
}
