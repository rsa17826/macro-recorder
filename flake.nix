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

        # Runtime shared libs gio's cgo backends (xkb, GL/EGL, Vulkan, Wayland, X11) link against.
        gioNativeDeps = with pkgs; [
          libxkbcommon
          wayland
          libX11
          libxcb
          libXcursor
          libXfixes
          libXi
          libGL
          libglvnd
          vulkan-loader
        ];

        # Extra build-time-only deps: C headers + .pc files needed to compile
        # the cgo code (not needed once the binary is built).
        gioBuildDeps = with pkgs; [
          vulkan-headers
          wayland-protocols
        ];
      in
      {
        packages = {
          default = pkgs.buildGoModule {
            pname = "macro-recorder";
            version = "1";
            src = ./.;
            vendorHash = "sha256-G3I61bfAJbfqSiYK5d5idNUF4mnmZ1wt0oX4+tZe32k=";

            nativeBuildInputs = [
              pkgs.pkg-config
              pkgs.patchelf
            ]
            ++ gioBuildDeps;
            buildInputs = gioNativeDeps;

            # gio's cgo code dlopen()s some of these at runtime too, so make
            # sure the built binary(ies) can find them even outside a devShell.
            postFixup = ''
              for f in $out/bin/*; do
                patchelf --set-rpath "${pkgs.lib.makeLibraryPath gioNativeDeps}" "$f"
              done
            '';
          };
        };
        devShells = {
          default = pkgs.mkShell {
            buildInputs =
              with pkgs;
              [
                go
                gopls
                pkg-config
              ]
              ++ gioNativeDeps
              ++ gioBuildDeps;

            LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath gioNativeDeps;
          };
        };
      }
    );
}
