#!/bin/bash
# Downloads the VictoriaMetrics and VictoriaLogs release binaries into
# state/bin, verifying each against the checksums file published beside it.
# Idempotent: a binary already reporting the pinned version is left alone.
# claude-obs install calls this; it is also safe to run on its own.
set -euo pipefail

OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# This resolution is right for the copy claude-obs install places in the
# install dir and wrong for the copy that stays in the source checkout, where
# it would download 44 MB of store binaries into the repository. go.mod tells
# the two apart: tracked in the checkout, never copied to an install dir.
# claude-obs install runs <install-dir>/bin/fetch-stores.sh, so the real path
# is unaffected.
if [ -f "$OBS_DIR/go.mod" ]; then
    echo "error: refusing to run from the source checkout ($OBS_DIR)." >&2
    echo "       these scripts run from an install dir; see: claude-obs install" >&2
    exit 1
fi
# shellcheck disable=SC1091
# ^ config.env is generated during install; this is the expected execution path
source "$OBS_DIR/config.env"

BIN_DIR="$OBS_DIR/state/bin"
: "${VM_VERSION:?config.env is missing VM_VERSION; regenerate it from config.env.example}"
: "${VL_VERSION:?config.env is missing VL_VERSION; regenerate it from config.env.example}"

log() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

case "$(uname -m)" in
  arm64)  ARCH=arm64 ;;
  x86_64) ARCH=amd64 ;;
  *) die "unsupported architecture: $(uname -m) (releases exist for arm64 and x86_64 only)" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# fetch_store REPO STEM BINARY VERSION
#   REPO    GitHub owner/repo holding the release
#   STEM    asset name without the -darwin-<arch>-<version>.tar.gz tail
#   BINARY  the file the tarball contains, and the name kept in state/bin
fetch_store() {
  local repo="$1" stem="$2" binary="$3" version="$4"
  local tarball="$stem-darwin-$ARCH-$version.tar.gz"
  local base="https://github.com/$repo/releases/download/$version"
  local dest="$BIN_DIR/$binary" work="$TMP/$binary" reported=""

  # -version is read into a variable rather than piped: piping into grep -q
  # can SIGPIPE the producer, and pipefail would then report a false mismatch.
  if [ -x "$dest" ]; then
    reported="$("$dest" -version 2>&1 || true)"
    if [[ "$reported" == *"$version"* ]]; then
      log "$binary $version already present"
      return 0
    fi
    log "$binary reports a different version; replacing it"
  fi

  mkdir -p "$work"
  log "downloading $tarball"
  curl -fsSL --retry 3 -o "$work/$tarball" "$base/$tarball" \
    || die "download failed: $base/$tarball"
  curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/${tarball%.tar.gz}_checksums.txt" \
    || die "checksum download failed: $base/${tarball%.tar.gz}_checksums.txt"

  local expected computed
  expected="$(grep -- "$tarball\$" "$work/checksums.txt" | awk '{print $1}')"
  [ -n "$expected" ] \
    || die "no checksum entry for $tarball in $base/${tarball%.tar.gz}_checksums.txt; nothing was installed"
  computed="$(shasum -a 256 "$work/$tarball" | awk '{print $1}')"
  [ "$expected" = "$computed" ] || die "checksum mismatch for $base/$tarball
  expected: $expected
  computed: $computed
nothing was installed"

  tar xzf "$work/$tarball" -C "$work" || die "could not extract $tarball"
  [ -f "$work/$binary" ] || die "$tarball did not contain $binary"

  # Verify, then move. A truncated download must never reach state/bin, where
  # launchd would exec it on a KeepAlive loop.
  mkdir -p "$BIN_DIR"
  chmod +x "$work/$binary"
  mv "$work/$binary" "$dest"
  log "installed $dest"
}

fetch_store VictoriaMetrics/VictoriaMetrics victoria-metrics victoria-metrics-prod "$VM_VERSION"
fetch_store VictoriaMetrics/VictoriaLogs    victoria-logs    victoria-logs-prod    "$VL_VERSION"
