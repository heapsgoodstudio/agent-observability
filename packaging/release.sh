#!/usr/bin/env bash
# Builds the release tarball for one version into dist/release: a universal
# claude-obs, for Apple Silicon and Intel Macs in one file, beside the licence
# and README, plus its SHA-256 sum. One file gives the Homebrew formula one
# download. Run it after `just build`, which embeds the frontend the binary
# carries. The sqlite driver is pure Go, so both architectures build on one
# Mac with cgo off.
#
#   packaging/release.sh v1.2.3
set -euo pipefail

tag="${1:?usage: packaging/release.sh v1.2.3}"
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "error: the version must look like v1.2.3, not $tag" >&2
    exit 1
fi
version="${tag#v}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$root/dist/release"
name="agent-observability-$version-macos"

if [ ! -f "$root/internal/server/dist/index.html" ]; then
    echo "error: the embedded frontend is missing; run just build first" >&2
    exit 1
fi

rm -rf "$out"
mkdir -p "$out/$name" "$out/arch"
for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go -C "$root" build -trimpath \
        -ldflags "-X main.version=$tag" -o "$out/arch/claude-obs-$arch" ./cmd/claude-obs
done
lipo -create -output "$out/$name/claude-obs" "$out/arch/claude-obs-arm64" "$out/arch/claude-obs-amd64"
# Go signs only the arm64 build; an ad-hoc signature over the merged file
# covers both slices.
/usr/bin/codesign --force --sign - "$out/$name/claude-obs"
/usr/bin/codesign --verify "$out/$name/claude-obs"
cp "$root/LICENSE" "$root/README.md" "$out/$name/"
tar -C "$out" -czf "$out/$name.tar.gz" "$name"
rm -rf "${out:?}/$name" "${out:?}/arch"
cd "$out"
shasum -a 256 -- *.tar.gz > checksums.txt
cat checksums.txt
