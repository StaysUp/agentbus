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
# and later), agentbus needs an AppArmor profile for the session wall,
# [network] contain and overlay copies without root. A profile is given only
# to a binary that only root can change, so it is never written for one in
# ~/.local/bin. Installing as root into such a folder offers it:
#
#   curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh
#
# AGENTBUS_APPARMOR=yes or no answers its question without asking. For a
# binary already in such a folder, the profile alone:
#
#   curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sudo sh -s apparmor /usr/local/bin/agentbus
#
# To take it away: sudo apparmor_parser -R /etc/apparmor.d/agentbus && sudo rm /etc/apparmor.d/agentbus
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

# open_to_others prints the first of $1 and the folders above it that a
# user other than root may change: not owned by root, or writable by its
# group or others. Nothing when only root can change them. Whoever can
# change a profile's path can give its user namespaces to any program.
open_to_others() {
  p=$(readlink -f "$1" 2>/dev/null) || { printf '%s\n' "$1"; return; }
  while :; do
    s=$(stat -L -c '%u %a' "$p" 2>/dev/null) || { printf '%s\n' "$p"; return; }
    [ "${s%% *}" = 0 ] || { printf '%s\n' "$p"; return; }
    case "${s#* }" in
      *[2367]? | *[2367]) printf '%s\n' "$p"; return ;;
    esac
    [ "$p" = / ] && return
    p=$(dirname "$p")
  done
}

# apparmor_profile prints the profile for the binary at $1: the profile in
# packaging/apparmor/agentbus, with its path.
apparmor_profile() {
  cat <<PROFILE
# AppArmor profile for agentbus, written by its install.sh: lets it make
# user namespaces, for the session wall, [network] contain and overlay
# copies without root. Its path must be one only root can change.

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
  # A profile's path is a pattern: it may hold only plain characters, so it
  # names this one file.
  case "$bin" in
    *[!A-Za-z0-9/._+-]*) fail "a profile cannot name $bin: its path may hold only letters, digits and / . _ + -" ;;
  esac
  open=$(open_to_others "$bin")
  [ -z "$open" ] || fail "a profile is given only to a binary that only root can change, and $open can be changed by another user. Install agentbus with its profile into /usr/local/bin instead:
  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
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
  say ""
  say "This system's AppArmor allows user namespaces only to a program with a profile. Without one,"
  say "agentbus cannot build the session wall or [network] contain, and a copy is a plain copy."
  if [ "$(id -u)" = 0 ] && [ -z "$(open_to_others "$dir/agentbus")" ]; then
    answer="${AGENTBUS_APPARMOR:-}"
    if [ -z "$answer" ] && [ -r /dev/tty ]; then
      printf 'Install the AppArmor profile /etc/apparmor.d/agentbus for %s? [y/N] ' "$dir/agentbus"
      read -r answer < /dev/tty || answer=
    fi
    case "$answer" in
      y | Y | yes) install_apparmor "$dir/agentbus" ;;
      *) say "Not installed. To install it later, run once:"
         say "  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo sh -s apparmor '$dir/agentbus'" ;;
    esac
  else
    say "A profile is given only to a binary that only root can change, so not to '$dir/agentbus'."
    say "To have them, install agentbus with its profile into /usr/local/bin, once:"
    say "  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
  fi
fi
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Note: $dir is not on your PATH. Add it, or run $dir/agentbus directly." ;;
esac
say ""
say "Next: agentbus up"
