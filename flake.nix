{
  description = "Agent Observability dev shell: pinned toolchain for build, test, install";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      forSystems = f: nixpkgs.lib.genAttrs [ "aarch64-darwin" "x86_64-darwin" ]
        (system: f nixpkgs.legacyPackages.${system});
    in {
      devShells = forSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_27
            nodejs_24
            just
            buf
            protoc-gen-go
            protoc-gen-connect-go
            shellcheck
            sqlite
          ];
        };
      });
    };
}
