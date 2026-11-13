#!/bin/sh
# Packs the cockpit into this binary's build: fills cockpit/dist, which
# cockpit/cockpit.go embeds, with the cockpit's `npm run build` output.
#
#   tools/pack-cockpit.sh v0.5.4        build the cockpit at that tag (a release: goreleaser runs this)
#   tools/pack-cockpit.sh --from DIR    copy DIR, a cockpit's dist folder you built yourself
#
# The tag build clones COCKPIT_REPO (default the public repository; a local
# checkout's path works too) into a temporary folder, runs npm ci and npm run
# build there, and copies dist. It needs git and Node.js 20.19+ or 22.12+.
# Git ignores everything in cockpit/dist but .keep, so a packed tree is
# still clean.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/cockpit/dist"
repo=${COCKPIT_REPO:-https://github.com/StaysUp/agentbus-cockpit.git}

usage() {
	echo "usage: tools/pack-cockpit.sh TAG | --from DIR" >&2
	exit 2
}

[ $# -ge 1 ] || usage
tmp=""
case "$1" in
--from)
	[ $# -eq 2 ] || usage
	src=$2
	;;
-*)
	usage
	;;
*)
	[ $# -eq 1 ] || usage
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	echo "pack-cockpit: building the cockpit at $1 from $repo"
	git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$1" "$repo" "$tmp/cockpit"
	(cd "$tmp/cockpit" && npm ci --no-audit --no-fund && npm run build)
	src="$tmp/cockpit/dist"
	;;
esac

if [ ! -f "$src/index.html" ]; then
	echo "pack-cockpit: $src has no index.html; is it a cockpit build?" >&2
	exit 1
fi
# Only the last build's files: Vite names them by content, so an old one
# would stay in the binary for nothing.
find "$out" -mindepth 1 ! -name .keep -exec rm -rf {} +
cp -R "$src/." "$out/"
echo "pack-cockpit: packed $(find "$out" -type f ! -name .keep | wc -l | tr -d ' ') files into cockpit/dist"
