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
# The archive is checked against the release's SHA256SUMS; with minisign installed, SHA256SUMS against its signature
# too (pperp upgrade always checks the signatures). Nothing runs until the whole script is here: main is the last line.
set -eu

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
  if command -v minisign >/dev/null 2>&1; then
    download "$base/v$version/SHA256SUMS.minisig" "$tmp/SHA256SUMS.minisig"
    download "$base/keys/pperp.pub" "$tmp/pperp.pub"
    minisign -Vqm "$tmp/SHA256SUMS" -p "$tmp/pperp.pub" || fail "SHA256SUMS is not signed by the release key: nothing was installed"
  fi
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
  if mkdir -p "$config"; then
    printf '{"installed_by":"install.sh","version":"%s","path":"%s"}\n' "$version" "$dir/pperp" > "$config/install.json" || true
  fi

  echo "pperp $version is installed in $dir."
  case ":$PATH:" in
    *":$dir:"*) echo "Next: pperp login --sandbox" ;;
    *) echo "Add $dir to your PATH (for example in ~/.profile: export PATH=\"$dir:\$PATH\"), then: pperp login --sandbox" ;;
  esac
}

main "$@"
