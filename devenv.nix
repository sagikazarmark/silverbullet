{ pkgs, ... }:

{
  packages = with pkgs; [
    devbox
  ];

  languages = {
    deno = {
      enable = true;
    };

    typescript = {
      enable = true;
    };
  };
}
