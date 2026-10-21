#!/bin/bash
# Builds the notifier app bundle and installs it to NOTIFIER_APP. Idempotent:
# rerunning rebuilds and atomically replaces the installed bundle.
#
# The bundle cannot live in this checkout. macOS refuses notification
# authorization to bundles outside a real applications directory: the identical
# bundle fails with UNErrorDomain Code=1 from the repo and from /tmp, and
# succeeds from ~/Applications. Hence NOTIFIER_APP.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
# ^ config.env is written by claude-obs install; this is the expected execution path
source "$HERE/../config.env"

# This bundle must build with the SYSTEM Swift toolchain (see flake.nix: nix
# Swift on macOS links UserNotifications poorly). A nix dev shell breaks that
# two ways: it exports SDKROOT/DEVELOPER_DIR pointing at the nix store's
# apple-sdk, which the system swiftc refuses ("this SDK is not supported by
# the compiler"), and it shadows xcrun with xcbuild's shim. Scrub the env and
# pin the real xcrun so `just install` works from inside the dev shell.
unset SDKROOT DEVELOPER_DIR
XCRUN=/usr/bin/xcrun

LABEL_PREFIX="${LABEL_PREFIX:-com.agent-observability}"
NOTIFIER_APP="${NOTIFIER_APP:-$HOME/Applications/Agent Observability.app}"
LSREGISTER=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister

log() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ -n "$NOTIFIER_APP" ] || { log "NOTIFIER_APP is empty; nothing to build"; exit 0; }
case "$NOTIFIER_APP" in
  /*) ;;
  *) die "NOTIFIER_APP must be an absolute path: $NOTIFIER_APP" ;;
esac
"$XCRUN" --find swiftc >/dev/null 2>&1 || die "swiftc not found; install the Xcode Command Line Tools"

# Skip the whole rebuild when nothing that goes into the bundle has changed.
#
# This is not a speed optimisation, it is a permissions one. The bundle is
# ad-hoc signed (`codesign --sign -` below), and an ad-hoc signature has no
# team identifier: its identity IS the code directory hash, which changes on
# every recompile. macOS keys TCC grants to that identity, so rebuilding hands
# the system a different app wearing the same name and every permission the
# notifier had is requested again. Four upgrades in one afternoon on
# 2026-09-21 meant four rounds of prompts for an app whose source had not
# changed once.
#
# The stamp covers everything that reaches the bundle: both Swift sources, the
# Info.plist template and the label that is substituted into it. It lives
# inside the bundle, so a hand-deleted app rebuilds, and a stamp that cannot be
# read rebuilds too. Not knowing always means build.
#
# CLAUDE_OBS_FORCE_NOTIFIER=1 rebuilds regardless, for when the toolchain
# changed underneath rather than the sources.
STAMP_REL="Contents/Resources/.build-stamp"
build_stamp() {
    cat "$HERE/main.swift" "$HERE/icon.swift" "$HERE/Info.plist.tmpl" 2>/dev/null \
        | shasum -a 256 | cut -d' ' -f1 | tr -d '\n'
    printf ' %s' "$LABEL_PREFIX"
}
want_stamp="$(build_stamp)"
if [ "${CLAUDE_OBS_FORCE_NOTIFIER:-0}" != 1 ] \
   && [ -x "$NOTIFIER_APP/Contents/MacOS/AgentObsNotifier" ] \
   && [ "$(cat "$NOTIFIER_APP/$STAMP_REL" 2>/dev/null)" = "$want_stamp" ]; then
    log "notifier: unchanged, keeping the installed bundle and its permissions"
    exit 0
fi

parent="$(dirname "$NOTIFIER_APP")"
mkdir -p "$parent"
# Stage beside the target so the final move is a same-filesystem rename.
staging="$parent/.claude-obs-build.$$"
trap 'rm -rf "$staging"' EXIT
rm -rf "$staging"
mkdir -p "$staging/Contents/MacOS" "$staging/Contents/Resources"

log "notifier: rendering Info.plist ($LABEL_PREFIX.notifier)"
sed -e "s|{{LABEL_PREFIX}}|$LABEL_PREFIX|g" "$HERE/Info.plist.tmpl" > "$staging/Contents/Info.plist"
plutil -lint "$staging/Contents/Info.plist" >/dev/null

log "notifier: compiling"
"$XCRUN" swiftc -O -o "$staging/Contents/MacOS/AgentObsNotifier" "$HERE/main.swift"

log "notifier: drawing icon"
iconbin="$staging/.icon-generator"
iconset="$staging/icon.iconset"
"$XCRUN" swiftc -O -o "$iconbin" "$HERE/icon.swift"
"$iconbin" "$iconset"
iconutil -c icns "$iconset" -o "$staging/Contents/Resources/icon.icns"
rm -rf "$iconset" "$iconbin"

# Written before signing so it is covered by the signature and by the sealed
# resources: a stamp that could be edited without invalidating the signature
# would let a tampered bundle claim to be current.
printf '%s' "$want_stamp" > "$staging/$STAMP_REL"

log "notifier: signing"
# Ad-hoc is sufficient: UNUserNotificationCenter accepts it, no Developer ID
# and no notarization required.
codesign --force --sign - "$staging" >/dev/null 2>&1 || die "codesign failed"

log "notifier: installing to $NOTIFIER_APP"
rm -rf "$NOTIFIER_APP"
mv "$staging" "$NOTIFIER_APP"
trap - EXIT
[ -x "$LSREGISTER" ] && "$LSREGISTER" -f "$NOTIFIER_APP" || true

log "notifier: built"
