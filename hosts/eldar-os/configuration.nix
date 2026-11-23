{ pkgs, ... }:
{
  networking.hostName = "eldar-os";
  time.timeZone = "America/New_York";
  i18n.defaultLocale = "en_US.UTF-8";

  boot.loader.systemd-boot.enable = true;
  boot.loader.systemd-boot.configurationLimit = 20;
  # No NVRAM boot entries: from the VM they'd land in QEMU's virtual NVRAM, not
  # the motherboard's. systemd-boot also installs \EFI\BOOT\BOOTX64.EFI, which
  # the real firmware boots from the disk without an entry.
  boot.loader.efi.canTouchEfiVariables = false;

  # Kernel messages on serial too, so the VM can be debugged headless. tty0 is
  # last, so it stays /dev/console: on bare metal the LUKS prompt must show on
  # screen, not on a serial port nobody is watching.
  #
  # no-kvmapf: while eldar-os runs as a VM, a host that swaps out its memory
  # sends async page faults, and one landing in kernel code panics the guest
  # ("Host injected async #PF in kernel mode"). Without them the guest just
  # waits for the page. No effect on bare metal.
  boot.kernelParams = [ "console=ttyS0,115200n8" "console=tty0" "no-kvmapf" ];

  nix.settings = {
    experimental-features = [ "nix-command" "flakes" ];
    trusted-users = [ "root" "@wheel" ];
  };

  networking.networkmanager.enable = true;

  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      PermitRootLogin = "no";
    };
  };

  programs.zsh = {
    enable = true;
    # Users' zsh setup (oh-my-zsh: completion, prompt) comes from their
    # dotfiles; don't run NixOS's versions first.
    enableGlobalCompInit = false;
    promptInit = "";
  };
  users.users.kevin = {
    isNormalUser = true;
    uid = 1000;
    extraGroups = [ "wheel" "networkmanager" ];
    shell = pkgs.zsh;
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICwOC/riI1EHGgX9P/gTedDS7XV16QIzSzmWO9w9i5rt kevin@shedquarters-pc"
    ];
  };

  # Remove before cutover. While the machine is only reached as kevin over SSH
  # (the admin session), passwordless sudo costs little and keeps admin work
  # quick. Once kevin runs a desktop, a browser and arbitrary tools, it would turn
  # any compromise of those into root. Agents are separate users either way.
  security.sudo.wheelNeedsPassword = false;

  # scripts/try: kevin may *activate* (not make the boot default) a config from
  # his scratch dir without a password: the admin's fast inner loop. Root-
  # equivalent for kevin, who is the admin anyway; agents run as other users and
  # only get changes applied through signed main (docs/decisions/0005).
  security.sudo.extraRules = [{
    users = [ "kevin" ];
    commands = [{
      command = "/run/current-system/sw/bin/nixos-rebuild test --flake /home/kevin/.cache/eldar-os-try\\#eldar-os";
      options = [ "NOPASSWD" ];
    } {
      # same, with a local dotfiles checkout in place of the pinned one
      command = "/run/current-system/sw/bin/nixos-rebuild test --flake /home/kevin/.cache/eldar-os-try\\#eldar-os --override-input dotfiles /home/kevin/.cache/eldar-os-try-dotfiles";
      options = [ "NOPASSWD" ];
    }];
  }];

  environment.systemPackages = with pkgs; [
    git vim curl rsync xfsprogs cryptsetup
    # 1Password CLI for the admin (the Eldar-OS vault); the supervisor's build
    (callPackage ../../packages/op-beta.nix { })
  ];

  zramSwap.enable = true;
  services.fstrim.enable = true;

  system.stateVersion = "26.05";
}
