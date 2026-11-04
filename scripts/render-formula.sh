#!/usr/bin/env bash
# Render the Homebrew formula for a release.
#
#   scripts/render-formula.sh dist/checksums.txt 1.2.3
#
# Writes dist/crossmap.rb with the source tarball's URL and checksum filled
# in. The formula builds from source, so it points at the tag's tarball
# rather than at a release binary.
set -euo pipefail

checksums=${1:?usage: render-formula.sh <checksums.txt> <version>}
version=${2:?usage: render-formula.sh <checksums.txt> <version>}
repo=${REPO:-wtfchewy/crossmap}
template=$(dirname "$0")/../packaging/homebrew/crossmap.rb.tmpl
out=$(dirname "$checksums")/crossmap.rb

url="https://github.com/${repo}/archive/refs/tags/v${version}.tar.gz"

# GitHub's source tarball is generated on demand, so its checksum is not in
# goreleaser's checksums file; fetch and hash it.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp"
sha=$(shasum -a 256 "$tmp" | cut -d' ' -f1)

sed -e "s|@URL@|${url}|" -e "s|@SHA256@|${sha}|" "$template" > "$out"
echo "wrote $out (v${version}, sha256 ${sha})"
