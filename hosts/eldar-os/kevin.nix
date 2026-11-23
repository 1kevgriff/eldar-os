# Kevin: the admin's account and tools. No dotfiles are applied to it yet
# (eldar-os.chezmoi.users.kevin.source would point at a dotfiles repo).
{ pkgs, ... }:
{
  users.users.kevin.packages = with pkgs; [
    tmux
    fzf
    gh
    helix
    ripgrep
    fd
    jq
    bat
    htop
    tree
    xxd # used by code-remote in .zshrc
    (callPackage ../../packages/herdr.nix { }) # same build as Eldar's VM
  ];

  # Claude Code on the host, for Kevin working on the system itself; installed
  # and self-updating as in Eldar's VM (eldar-vm.nix).
  environment.localBinInPath = true;
  programs.nix-ld.enable = true;
  systemd.services.claude-install-kevin = {
    description = "Install Claude Code for kevin";
    wantedBy = [ "multi-user.target" ];
    wants = [ "network-online.target" ];
    after = [ "network-online.target" ];
    unitConfig.ConditionPathExists = "!/home/kevin/.local/bin/claude";
    path = with pkgs; [ bash curl coreutils gnutar gzip ];
    environment.HOME = "/home/kevin";
    serviceConfig = {
      Type = "oneshot";
      User = "kevin";
      ExecStart = "${pkgs.bash}/bin/bash -c 'curl -fsSL https://claude.ai/install.sh | bash'";
    };
  };
}
