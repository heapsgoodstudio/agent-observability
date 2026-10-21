#!/usr/bin/env bash
# Renders the Homebrew formula for one release from the checksums
# packaging/release.sh wrote, and pushes it to heapsgoodstudio/homebrew-tap.
# TAP_TOKEN must be a token allowed to push to that repository.
#
#   TAP_TOKEN=... packaging/update-tap.sh v1.2.3
set -euo pipefail

tag="${1:?usage: packaging/update-tap.sh v1.2.3}"
: "${TAP_TOKEN:?TAP_TOKEN must be set to a token that can push to heapsgoodstudio/homebrew-tap}"
version="${tag#v}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
sums="$root/dist/release/checksums.txt"

sha256="$(awk -v file="agent-observability-$version-macos.tar.gz" '$2 == file { print $1 }' "$sums")"
if [ -z "$sha256" ]; then
    echo "error: $sums has no checksum for $version" >&2
    exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git clone -q --depth 1 "https://x-access-token:$TAP_TOKEN@github.com/heapsgoodstudio/homebrew-tap.git" "$work/tap"
mkdir -p "$work/tap/Formula"
sed -e "s/{{VERSION}}/$version/g" -e "s/{{SHA256}}/$sha256/g" \
    "$root/packaging/homebrew/agent-observability.rb.tmpl" > "$work/tap/Formula/agent-observability.rb"
git -C "$work/tap" add Formula/agent-observability.rb
if git -C "$work/tap" diff --cached --quiet; then
    echo "the tap already has agent-observability $version"
    exit 0
fi
git -C "$work/tap" -c user.name="github-actions[bot]" \
    -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
    commit -q -m "agent-observability $version"
git -C "$work/tap" push -q origin HEAD
echo "pushed agent-observability $version to heapsgoodstudio/homebrew-tap"
