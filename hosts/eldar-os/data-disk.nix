# The data disk: on dbmart, the WD Blue SN580 1TB (serial 244432808033), for
# bulky data that can be rebuilt, not the OS; Eldar's VM images live here.
# eldar-os runs as a VM on that host, so scripts/vm passes the drive through
# as virtio with serial "eldar-data" (a sparse image in a dry run), and that
# is the name used here. Machine-specific: a fork points it at its own drive.
#
# One partition, LUKS2 around XFS, mounted at /data. The key is a sops secret
# that stage 2's crypttab reads (sops-nix decrypts during activation, before
# any crypttab unit starts); there's no passphrase, so the drive only unlocks
# on this machine. Formatted once by hand (docs/install.md, "The data disk");
# scripts/e2e formats the dry-run image the same way with a throwaway key.
#
# `nofail` with short timeouts: a missing or locked drive should never hold up
# boot. Anything that keeps its state here must require the mount itself, so
# it waits for the drive or doesn't start, instead of writing into an empty
# /data.
{ config, ... }:
let
  luksDevice = "/dev/disk/by-id/virtio-eldar-data-part1";
in
{
  # 64 characters, no newline.
  sops.secrets.data-disk-key = { };

  environment.etc.crypttab.text = ''
    data ${luksDevice} ${config.sops.secrets.data-disk-key.path} luks,discard,nofail,x-systemd.device-timeout=10s
  '';

  fileSystems."/data" = {
    device = "/dev/mapper/data";
    fsType = "xfs";
    options = [ "noatime" "nofail" "x-systemd.device-timeout=10s" ];
  };
}
