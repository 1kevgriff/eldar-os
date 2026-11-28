# The supervisor on this machine (modules/supervisor.nix). Its policy and
# 1Password configuration are private: they come from the secrets repo.
{ config, pkgs, ... }:
{
  eldar-os.supervisor = {
    enable = true;
    admins = [ "kevin" ];
    # Lets a Stream Deck focus a tab in Kevin's herdr here (docs/supervisor.md).
    hostAgent = { user = "kevin"; path = [ (pkgs.callPackage ../../packages/herdr.nix { }) ]; };
    policyFile = "/var/lib/eldar-os/secrets/supervisor/policy.yaml";
    environmentFile = config.sops.secrets.supervisor-env.path;
    # Other machines agents may run commands on (none yet; each also needs a
    # sops secret remote-hosts/<name> with its SSH key).
    hostsFile = "/var/lib/eldar-os/secrets/supervisor/hosts.json";
    # AWS profiles (one SSO session; `supervisor aws-login` signs in).
    awsConfig = "/var/lib/eldar-os/secrets/supervisor/aws-config";
    ambientFile = "/var/lib/eldar-os/secrets/supervisor/ambient.yaml";
    # The web UI's Jira tab: Kevin's work tickets and sprint.
    jiraFile = "/var/lib/eldar-os/secrets/supervisor/jira.json";
    web = {
      enable = true;
      hostname = "supervisor";
      streamHost = config.eldar-os.tailnetProxies.chrome.hostname; # the Browser tab
      environmentFile = config.sops.secrets.supervisor-web-env.path;
    };
    # Kevin's Grok bot hands Eldar tasks over MCP and hears back by webhook;
    # it can't approve anything (cmd/bot-bridge).
    bot = {
      enable = true;
      source = "grokbot";
      environmentFile = config.sops.secrets.bot-env.path;
    };
  };
  sops.secrets.bot-env = {
    owner = "eldar-bot";
    restartUnits = [ "eldar-bot.service" ];
  };
  # Kevin watches the VM's Chrome (modules/vm-chrome.nix) from his tailnet.
  eldar-os.tailnetProxies.chrome = {
    hostname = "eldar-chrome";
    target = "http://${config.eldar-os.eldarVm.address}:3000";
    frameHosts = [ config.eldar-os.supervisor.web.hostname ]; # the web UI's Browser tab
    environmentFile = config.sops.secrets.supervisor-web-env.path;
  };
  sops.secrets.supervisor-web-env = {
    owner = "supervisor-web";
    restartUnits = [ "supervisor-web.service" ];
  };
  sops.secrets.supervisor-env = {
    owner = "supervisor";
    restartUnits = [ "supervisor.service" ];
  };
  # Eldar's GitHub App (docs/decisions/0011-eldar-github-app.md); its IDs
  # are in supervisor-env.
  sops.secrets.github-app-key = {
    owner = "supervisor";
    restartUnits = [ "supervisor.service" ];
  };
  systemd.services.supervisor.environment.SUPERVISOR_GITHUB_APP_KEY = config.sops.secrets.github-app-key.path;
}
