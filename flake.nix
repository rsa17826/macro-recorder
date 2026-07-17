{
  inputs = {
    nixpkgs = {
      url = "github:NixOS/nixpkgs/nixos-unstable";
    };
    flake-utils = {
      url = "github:numtide/flake-utils";
    };
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages = {
          default = pkgs.buildGoModule {
            pname = "macro-recorder";
            version = "1";
            src = ./.;
            vendorHash = "sha256-G3I61bfAJbfqSiYK5d5idNUF4mnmZ1wt0oX4+tZe32k=";
          };
        };
        devShells = {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
            ];
          };
        };
      }
    );
}
