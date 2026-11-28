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
# and later), agentbus needs an AppArmor profile for a Copy session's command
# wall, its network allow-list and overlay copies without root. A profile is given only
# to a binary that only root can change, so it is never written for one in
# ~/.local/bin. Installing as root on such a system puts the binary in
# /usr/local/bin (unless AGENTBUS_INSTALL_DIR says otherwise), offers the
# profile, and names any other agentbus that would run without it:
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
# The wall around a turn's processes is srt (sandbox-runtime), at the version
# in packaging/srt. It needs Node.js 22.12 or later, and on Linux bwrap, socat
# and ripgrep. install.sh installs srt with npm ci from that lockfile into
# <install dir>/../lib/agentbus-srt and links srt next to agentbus. As root on
# Linux it installs bwrap, socat and ripgrep too; otherwise it prints the
# command. A missing piece never stops the install: the runner probes the wall
# and reports what is missing. AGENTBUS_WALL=no skips this step.
#
set -eu

repo="staysup/agentbus"
dir="${AGENTBUS_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
fail() { printf 'agentbus install: %s\n' "$*" >&2; exit 1; }

# path_hint prints the line to add for this user's shell. It only prints:
# install.sh never edits a profile file.
path_hint() {
  case "$(basename "${SHELL:-}")" in
    zsh)
      say "  echo 'export PATH=\"$1:\$PATH\"' >> ~/.zshrc"
      say "  Then open a new terminal, or run: source ~/.zshrc"
      ;;
    bash)
      # Keep the tilde literal in the printed line. An expanded absolute path
      # is correct but harder to recognise and copy on another machine.
      profile='~/.bashrc'
      if [ "$(uname -s)" = Darwin ]; then profile='~/.bash_profile'; fi
      say "  echo 'export PATH=\"$1:\$PATH\"' >> $profile"
      say "  Then open a new terminal, or run: source $profile"
      ;;
    fish)
      say "  fish_add_path $1"
      ;;
    *)
      say "  Add $1 to your shell's PATH."
      ;;
  esac
}

# needs_apparmor: this system lets a program make an unprivileged user
# namespace only under an AppArmor profile that allows it.
needs_apparmor() {
  [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)" = 1 ]
}

# plain_path: $1 holds only the characters a profile's path may hold.
# AppArmor reads that path as a pattern, and these are never part of one.
# netjail.ProfilePathChars is the same set, and a test checks both agree.
# Spelled out, not as ranges: A-Z under a UTF-8 locale may take in ü.
plain_path() {
  case "$1" in
    "" | *[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-]*) return 1 ;;
  esac
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

# other_binaries names each other agentbus that runs without the profile
# of $1 when it comes first on PATH: the one on this PATH, and the one in
# the sudo user's ~/.local/bin, the default without sudo. It names a path it
# found in quotes and never prints a command that acts on it: a path may
# hold a space or anything else a shell reads.
other_binaries() {
  others=$(command -v agentbus 2>/dev/null || true)
  if [ -n "${SUDO_USER:-}" ]; then
    home=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6)
    [ -z "$home" ] || others="$others
$home/.local/bin/agentbus"
  fi
  printf '%s\n' "$others" | while IFS= read -r o; do
    [ -n "$o" ] && [ -e "$o" ] || continue
    [ "$(readlink -f "$o" 2>/dev/null)" = "$1" ] && continue
    say "Another agentbus, at '$o', has no profile, so the wall stays off when it comes first on PATH."
    say "Remove the agentbus at '$o', or run $1 directly."
  done
}

# srt_version is the srt that packaging/srt pins; wall.Version in the code.
srt_version=0.0.83

# node_ok says the Node.js version $1 (as node --version prints it) is
# 22.12 or later, which srt needs.
node_ok() {
  v=${1#v}
  major=${v%%.*}
  rest=${v#*.}
  minor=${rest%%.*}
  case "$major$minor" in '' | *[!0-9]*) return 1 ;; esac
  [ "$major" -gt 22 ] || { [ "$major" -eq 22 ] && [ "$minor" -ge 12 ]; }
}

# linux_wall_tools installs bwrap, socat and ripgrep as root with the
# system's package manager, or prints the command.
linux_wall_tools() {
  missing=
  for t in bwrap socat rg; do command -v "$t" >/dev/null 2>&1 || missing="$missing $t"; done
  [ -n "$missing" ] || return 0
  if command -v apt-get >/dev/null 2>&1; then cmd="apt-get install -y bubblewrap socat ripgrep"
  elif command -v dnf >/dev/null 2>&1; then cmd="dnf install -y bubblewrap socat ripgrep"
  elif command -v pacman >/dev/null 2>&1; then cmd="pacman -S --noconfirm bubblewrap socat ripgrep"
  else
    say "The wall needs bwrap, socat and ripgrep, missing here:$missing. Install them with your package manager."
    return 0
  fi
  if [ "$(id -u)" = 0 ]; then
    say "Installing bwrap, socat and ripgrep for the wall"
    $cmd >/dev/null || say "Could not install them; run: $cmd"
  else
    say "The wall needs bwrap, socat and ripgrep, missing here:$missing. Install them once:"
    say "  sudo $cmd"
  fi
}

# install_srt installs the pinned srt next to the binary in $1: npm ci from
# packaging/srt at the release's tag, into $1/../lib/agentbus-srt,
# and srt linked into $1. It says what is missing and returns, never fails.
install_srt() {
  # Absolute, so the link's target holds from inside $1 (AGENTBUS_INSTALL_DIR
  # may be relative).
  bindir=$(cd "$1" && pwd -P) || { say "Cannot read $1; srt is not installed."; return 0; }
  if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
    say "The wall needs srt, which runs on Node.js 22.12 or later; node and npm are not on PATH."
    say "Install Node.js (macOS: brew install node; or https://nodejs.org), then run this script again."
    return 0
  fi
  if ! node_ok "$(node --version 2>/dev/null)"; then
    say "The wall needs srt, which runs on Node.js 22.12 or later; this node is $(node --version 2>/dev/null)."
    say "Install a newer Node.js (macOS: brew install node; or https://nodejs.org), then run this script again."
    return 0
  fi
  lib="$(dirname "$bindir")/lib/agentbus-srt"
  src="https://raw.githubusercontent.com/$repo/$srt_ref/packaging/srt"
  if ! mkdir -p "$tmp/srt" ||
    ! curl -fsSL -o "$tmp/srt/package.json" "$src/package.json" ||
    ! curl -fsSL -o "$tmp/srt/package-lock.json" "$src/package-lock.json"; then
    say "Could not download $src; srt is not installed."
    return 0
  fi
  say "Installing srt $srt_version (sandbox-runtime) for the wall"
  if ! mkdir -p "$lib" 2>/dev/null || [ ! -w "$lib" ]; then
    say "Cannot write to $lib; srt is not installed."
    return 0
  fi
  cp "$tmp/srt/package.json" "$tmp/srt/package-lock.json" "$lib/"
  if ! npm ci --prefix "$lib" --omit=dev --no-fund --no-audit >/dev/null 2>&1; then
    say "npm ci in $lib failed; srt is not installed. Run it there to see why."
    return 0
  fi
  ln -sf "$lib/node_modules/.bin/srt" "$bindir/srt"
  got=$("$bindir/srt" --version 2>/dev/null) || got=
  if [ "$got" = "$srt_version" ]; then
    say "Installed $bindir/srt $srt_version"
  else
    say "srt in $lib did not report $srt_version; the runner will say what is wrong."
  fi
}

# apparmor_profile prints the profile for the binary at $1: the profile in
# packaging/apparmor/agentbus, with its path.
apparmor_profile() {
  cat <<PROFILE
# AppArmor profile for agentbus, written by its install.sh: lets it make
# user namespaces, for a Copy session's command wall, its network allow-list
# and overlay copies without root. Its path must be one only root can change.

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
  plain_path "$bin" || fail "a profile cannot name $bin: its path may hold only letters, digits and / . _ + -. Install agentbus with its profile into /usr/local/bin instead:
  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
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

# Where AppArmor restricts user namespaces, the wall needs a profile, and a
# profile is given only to a binary only root can change: as root, the
# binary goes in /usr/local/bin, not in root's own ~/.local/bin.
if [ -z "${AGENTBUS_INSTALL_DIR:-}" ] && [ "$os" = linux ] && [ "$(id -u)" = 0 ] && needs_apparmor; then
  dir=/usr/local/bin
fi

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
profiled=
if [ "$os" = linux ] && needs_apparmor; then
  say ""
  say "This system's AppArmor allows user namespaces only to a program with a profile. Without one,"
  say "agentbus cannot build a Copy session's command wall or its network allow-list, and a copy is a plain copy."
  # The path checked, printed and given the profile is one: the binary's
  # own, links resolved, so a command printed here is the one that passed.
  bin=$(readlink -f "$dir/agentbus" 2>/dev/null) || bin=
  if [ "$(id -u)" = 0 ] && plain_path "$bin" && [ -z "$(open_to_others "$bin")" ]; then
    answer="${AGENTBUS_APPARMOR:-}"
    if [ -z "$answer" ] && [ -r /dev/tty ]; then
      printf 'Install the AppArmor profile /etc/apparmor.d/agentbus for %s? [y/N] ' "$bin"
      read -r answer < /dev/tty || answer=
    fi
    case "$answer" in
      y | Y | yes) install_apparmor "$bin"; profiled=$bin ;;
      *) say "Not installed. To install it later, run once:"
         say "  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo sh -s apparmor $bin" ;;
    esac
  else
    say "A profile is given only to a binary that only root can change, at a path of plain characters,"
    say "so not to $dir/agentbus."
    say "To have them, install agentbus with its profile into /usr/local/bin, once:"
    say "  curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
  fi
  [ -z "$profiled" ] || other_binaries "$profiled"
fi
if [ "${AGENTBUS_WALL:-yes}" != no ]; then
  say ""
  # packaging/srt is read at the release's tag, so srt is the version that
  # release's runner probes for.
  srt_ref="v$version"
  install_srt "$dir"
  [ "$os" != linux ] || linux_wall_tools
fi
case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    say "Note: $dir is not on your PATH. Add the line below, or run $dir/agentbus directly."
    path_hint "$dir"
    ;;
esac
say ""
if [ -n "$profiled" ]; then
  say "Next: $profiled up"
else
  say "Next: agentbus up"
fi
