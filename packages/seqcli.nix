{ stdenv, lib, fetchurl, autoPatchelfHook, makeWrapper, icu, openssl, zlib }:

# Seq's command-line client, from upstream's self-contained .NET release.
# nixpkgs' seq-cli is years behind and lacks flags agents use (search
# --column, --native). To bump: change `version` and replace `hash`
# (nix-prefetch-url $url, then `nix hash convert --to sri`).
stdenv.mkDerivation rec {
  pname = "seqcli";
  version = "2026.2.02674";

  src = fetchurl {
    url = "https://github.com/datalust/seqcli/releases/download/v${version}/seqcli-${version}-linux-x64.tar.gz";
    hash = "sha256-Zj6F/p0SPG3p7KeqrMrrwUeSjKAInFQB6DHlmCNF1Sg=";
  };
  sourceRoot = "seqcli-${version}-linux-x64";

  nativeBuildInputs = [ autoPatchelfHook makeWrapper ];
  buildInputs = [ stdenv.cc.cc.lib icu openssl zlib ];
  # .NET's optional tracing library; seqcli runs without it.
  autoPatchelfIgnoreMissingDeps = [ "liblttng-ust.so.0" "liblttng-ust.so.1" ];
  dontStrip = true; # stripping breaks .NET single-file hosts

  installPhase = ''
    runHook preInstall
    mkdir -p $out/lib/seqcli
    cp -r ./* $out/lib/seqcli/
    makeWrapper $out/lib/seqcli/seqcli $out/bin/seqcli \
      --prefix LD_LIBRARY_PATH : ${lib.makeLibraryPath [ icu openssl ]}
    runHook postInstall
  '';

  meta = {
    description = "Seq command-line client";
    homepage = "https://github.com/datalust/seqcli";
    license = lib.licenses.asl20;
    platforms = [ "x86_64-linux" ];
    mainProgram = "seqcli";
  };
}
