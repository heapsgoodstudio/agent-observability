#!/usr/bin/env bash
# Renders the Homebrew formula for one release from the checksums
# packaging/release.sh wrote, and pushes it to heapsgoodstudio/homebrew-tap.
# TAP_DEPLOY_KEY is the private half of an SSH deploy key with write access to
# that repository and no other.
#
#   TAP_DEPLOY_KEY="$(cat key)" packaging/update-tap.sh v1.2.3
set -euo pipefail

tag="${1:?usage: packaging/update-tap.sh v1.2.3}"
: "${TAP_DEPLOY_KEY:?TAP_DEPLOY_KEY must hold the tap deploy key}"
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
(umask 077 && printf '%s\n' "$TAP_DEPLOY_KEY" > "$work/key")
export GIT_SSH_COMMAND="ssh -i $work/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
git clone -q --depth 1 git@github.com:heapsgoodstudio/homebrew-tap.git "$work/tap"
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
