#!/usr/bin/env bash
# Render the Homebrew formula for a release.
#
#   scripts/render-formula.sh <version> [outdir]
#   scripts/render-formula.sh 1.2.3 dist
#
# Reads the per-architecture checksums from the release's checksums.txt and
# writes <outdir>/crossmap.rb. The formula installs the prebuilt binaries, so
# it needs no Go toolchain on the installing machine.
set -euo pipefail

version=${1:?usage: render-formula.sh <version> [outdir]}
version=${version#v}
outdir=${2:-dist}
repo=${REPO:-wtfchewy/crossmap}
template=$(cd "$(dirname "$0")/.." && pwd)/packaging/homebrew/crossmap.rb.tmpl
out=$outdir/crossmap.rb

mkdir -p "$outdir"

checksums=$outdir/checksums.txt
if [ ! -s "$checksums" ]; then
  curl -fsSL "https://github.com/${repo}/releases/download/v${version}/checksums.txt" -o "$checksums"
fi

sha_for() {
  awk -v want="crossmap_${version}_darwin_$1.tar.gz" '$2 == want { print $1 }' "$checksums"
}

arm64=$(sha_for arm64)
amd64=$(sha_for amd64)
: "${arm64:?no arm64 checksum in $checksums}"
: "${amd64:?no amd64 checksum in $checksums}"

sed -e "s|@VERSION@|${version}|g" \
    -e "s|@SHA256_ARM64@|${arm64}|" \
    -e "s|@SHA256_AMD64@|${amd64}|" \
    "$template" > "$out"

echo "wrote $out (v${version})"
echo "  arm64 ${arm64}"
echo "  amd64 ${amd64}"
