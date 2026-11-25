#!/bin/sh
# Installs pperp, the Phoenix Perpetuals command line, for the current user (never with sudo):
#
#   curl -fsSL https://docs.phoenixperpsfunding.com/install.sh | sh
#
# PPERP_VERSION=1.2.0     that version instead of the latest
# PPERP_INSTALL_DIR=DIR   where it goes (default ~/.local/bin)
#
# The PATH is never changed: the end says how, when the folder is not on it.
#
# The archive is checked against the release's SHA256SUMS, and SHA256SUMS against the release key pinned below (never
# fetched from the server the files come from): with openssl or LibreSSL (its P-256 signature), else minisign. With
# neither, nothing is installed unless PPERP_INSECURE=1 (the checksum only, said so). Nothing runs until the whole script
# is here: main is the last line.
set -eu

# BEGIN pinned release keys (go run ./tools/release installer-key writes them; tools/release build checks them)
P256_PEM='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEUuvZU2kqZcWr6e60ydf947uD7WQO
0DmoJ4ip6zfKbZnphPukumeOxrFi7Mxu6ohqm+u2eXADHAspL3JTVXgmRw==
-----END PUBLIC KEY-----'
MINISIGN_PUB='RWQ1BrEPEK9erW/k12JUZKOaMmm2/Iu0WH2Z6zBnn/1g/9qU+Vg/K9S2'
# END pinned release keys

fail() {
  echo "pperp: $*" >&2
  exit 1
}

download() { # url file
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto "=$PROTO" --tlsv1.2 -o "$2" "$1" || fail "could not download $1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1" || fail "could not download $1"
  else
    fail "curl or wget is needed"
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# SHA256SUMS against the pinned release key; the releases on this computer (the installers' own tests) may name their
# own P-256 key in PPERP_TEST_P256_PEM
check_signature() {
  pem="$P256_PEM"
  if [ "$PROTO" = http ] && [ -n "${PPERP_TEST_P256_PEM:-}" ]; then pem="$PPERP_TEST_P256_PEM"; fi
  if [ -n "$pem" ] && command -v openssl >/dev/null 2>&1; then
    download "$base/v$version/SHA256SUMS.p256.sig" "$tmp/SHA256SUMS.p256.sig"
    printf '%s\n' "$pem" > "$tmp/release.pem"
    openssl dgst -sha256 -verify "$tmp/release.pem" -signature "$tmp/SHA256SUMS.p256.sig" "$tmp/SHA256SUMS" >/dev/null 2>&1 ||
      fail "SHA256SUMS is not signed by the release key: nothing was installed"
    return 0
  fi
  if [ -n "$MINISIGN_PUB" ] && command -v minisign >/dev/null 2>&1; then
    download "$base/v$version/SHA256SUMS.minisig" "$tmp/SHA256SUMS.minisig"
    minisign -Vqm "$tmp/SHA256SUMS" -P "$MINISIGN_PUB" || fail "SHA256SUMS is not signed by the release key: nothing was installed"
    return 0
  fi
  if [ "${PPERP_INSECURE:-}" = 1 ]; then
    echo "pperp: WARNING: the release signature was NOT checked (no openssl or minisign here, PPERP_INSECURE=1): only the checksum was" >&2
    return 0
  fi
  fail "openssl or minisign is needed to check the release signature (PPERP_INSECURE=1 installs with the checksum only)"
}

main() {
  base="${PPERP_RELEASES_URL:-https://docs.phoenixperpsfunding.com/downloads/cli}"
  case "$base" in
    https://*) PROTO=https ;;
    http://127.0.0.1:*) PROTO=http ;; # this computer: the installers' own tests
    *) fail "the release address must be https" ;;
  esac
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "this installer is for macOS and Linux; on Windows: irm https://docs.phoenixperpsfunding.com/install.ps1 | iex" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail "no pperp for $(uname -m) yet" ;;
  esac

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  version="${PPERP_VERSION:-}"
  if [ -z "$version" ]; then
    download "$base/latest.json" "$tmp/latest.json"
    version="$(sed -n 's/.*"version" *: *"\([0-9][0-9.]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
    [ -n "$version" ] || fail "could not read the latest version"
  fi
  version="${version#v}"

  file="pperp_${version}_${os}_${arch}.tar.gz"
  download "$base/v$version/$file" "$tmp/$file"
  download "$base/v$version/SHA256SUMS" "$tmp/SHA256SUMS"
  check_signature
  want="$(awk -v f="$file" '$2 == f || $2 == "*"f { print $1 }' "$tmp/SHA256SUMS")"
  [ -n "$want" ] || fail "version $version has no build for $os/$arch"
  [ "$(sha256 "$tmp/$file")" = "$want" ] || fail "the download does not match its checksum: nothing was installed"

  tar -xzf "$tmp/$file" -C "$tmp" pperp || fail "the archive could not be opened"
  dir="${PPERP_INSTALL_DIR:-$HOME/.local/bin}"
  mkdir -p "$dir" || fail "cannot create $dir"
  if ! cp "$tmp/pperp" "$dir/.pperp.new" || ! chmod 755 "$dir/.pperp.new"; then
    fail "cannot write to $dir"
  fi
  "$dir/.pperp.new" version >/dev/null 2>&1 || { rm -f "$dir/.pperp.new"; fail "the new pperp does not run on this computer"; }
  mv -f "$dir/.pperp.new" "$dir/pperp"

  config="${XDG_CONFIG_HOME:-$HOME/.config}/pperp"
  if mkdir -p "$config" && chmod 700 "$config"; then # credentials may be kept in it: the user's only
    printf '{"installed_by":"install.sh","version":"%s","path":"%s"}\n' "$version" "$dir/pperp" > "$config/install.json" || true
  fi

  echo "pperp $version is installed in $dir."
  case ":$PATH:" in
    *":$dir:"*) echo "Next: pperp login --sandbox" ;;
    *) echo "Add $dir to your PATH (for example in ~/.profile: export PATH=\"$dir:\$PATH\"), then: pperp login --sandbox" ;;
  esac
}

main "$@"
