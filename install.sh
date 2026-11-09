#!/bin/sh
# Installs the agentbus binary from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sh
#
# Settings, all optional:
#   AGENTBUS_VERSION      a version such as 0.1.0; the latest release when unset
#   AGENTBUS_INSTALL_DIR  where to put the binary; ~/.local/bin when unset
#
# On Linux systems that restrict unprivileged user namespaces (Ubuntu 24.04
# and later), agentbus needs an AppArmor profile to make overlay copies
# without root. Installing as root installs it too. Otherwise the install
# ends by printing the one command to run as root:
#
#   curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sudo sh -s apparmor /path/to/agentbus
#
set -eu

repo="staysup/agentbus"
dir="${AGENTBUS_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
fail() { printf 'agentbus install: %s\n' "$*" >&2; exit 1; }

# needs_apparmor: this system lets a program make an unprivileged user
# namespace only under an AppArmor profile that allows it.
needs_apparmor() {
  [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)" = 1 ]
}

# apparmor_profile prints the profile for the binary at $1: the profile in
# packaging/apparmor/agentbus, with its path.
apparmor_profile() {
  cat <<PROFILE
# AppArmor profile for agentbus, written by its install.sh: lets it make its
# own user namespace, for overlay copies without root.

abi <abi/4.0>,
include <tunables/global>

profile agentbus "$1" flags=(unconfined) {
  userns,

  include if exists <local/agentbus>
}
PROFILE
}

# install_apparmor installs and loads the profile for the binary at $1, as
# root.
install_apparmor() {
  [ "$(id -u)" = 0 ] || fail "the AppArmor profile needs root: run it with sudo"
  bin=$(readlink -f "$1" 2>/dev/null) || fail "no agentbus binary at $1"
  { [ -f "$bin" ] && [ -x "$bin" ]; } || fail "no agentbus binary at $1"
  command -v apparmor_parser >/dev/null 2>&1 || fail "apparmor_parser is needed"
  [ -d /etc/apparmor.d ] || fail "/etc/apparmor.d does not exist"
  prof=$(mktemp)
  apparmor_profile "$bin" > "$prof"
  if ! apparmor_parser -Q -K "$prof" >/dev/null 2>&1; then
    rm -f "$prof"
    fail "this AppArmor cannot read the profile: it needs AppArmor 4 (Ubuntu 24.04 and later)"
  fi
  mv -f "$prof" /etc/apparmor.d/agentbus
  chmod 644 /etc/apparmor.d/agentbus
  apparmor_parser -r /etc/apparmor.d/agentbus || fail "the profile could not be loaded"
  say "Installed the AppArmor profile /etc/apparmor.d/agentbus for $bin"
}

if [ "${1:-}" = apparmor ]; then
  [ "$(uname -s)" = Linux ] || fail "AppArmor is Linux only"
  [ -n "${2:-}" ] || fail "usage: sh install.sh apparmor /path/to/agentbus"
  install_apparmor "$2"
  exit 0
fi

command -v curl >/dev/null 2>&1 || fail "curl is needed"
command -v tar >/dev/null 2>&1 || fail "tar is needed"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported system $(uname -s); macOS and Linux only" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported machine $(uname -m); amd64 and arm64 only" ;;
esac

if [ -n "${AGENTBUS_VERSION:-}" ]; then
  version="${AGENTBUS_VERSION#v}"
else
  # /releases/latest redirects to /releases/tag/v<version>.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
    fail "could not reach https://github.com/$repo/releases/latest"
  tag="${url##*/}"
  case "$tag" in
    v[0-9]*) version="${tag#v}" ;;
    *) fail "no release found at https://github.com/$repo/releases" ;;
  esac
fi

archive="agentbus_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/v$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading agentbus $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "could not download $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "could not download $base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
else
  fail "sha256sum or shasum is needed to check the download"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive; nothing installed"

tar -xzf "$tmp/$archive" -C "$tmp" agentbus || fail "could not unpack $archive"

if ! mkdir -p "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
  fail "cannot write to $dir. Run again with AGENTBUS_INSTALL_DIR set to a folder you own, or with sudo for /usr/local/bin:
  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
fi

# Copy then rename, so a running agentbus is never half overwritten.
cp "$tmp/agentbus" "$dir/.agentbus.new"
chmod 755 "$dir/.agentbus.new"
mv -f "$dir/.agentbus.new" "$dir/agentbus"

say "Installed $dir/agentbus"
if [ "$os" = linux ] && needs_apparmor; then
  if [ "$(id -u)" = 0 ]; then
    install_apparmor "$dir/agentbus"
  else
    say "This system restricts user namespaces. So agentbus can make overlay copies without root, run once:"
    say "  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo sh -s apparmor $dir/agentbus"
    say "Without it, a session's copy is a plain copy of its folder."
  fi
fi
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Note: $dir is not on your PATH. Add it, or run $dir/agentbus directly." ;;
esac
say ""
say "Next: agentbus up"
