#!/bin/bash
# Reads statusline stdin JSON on stdin; stashes rate_limits atomically. Fast, silent.
set -u
OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Refuse the source checkout, silently: this is forked on every statusline
# render, and stderr from a statusline command lands in the user's terminal.
# The other scripts say so out loud; this one cannot. go.mod is tracked in the
# checkout and never copied to an install dir. See bin/fetch-stores.sh.
[ -f "$OBS_DIR/go.mod" ] && exit 0
STATE="$OBS_DIR/state"

# Which profile this render belongs to. Claude Code exports CLAUDE_CONFIG_DIR
# to everything it spawns for a named profile, and leaves it unset for the
# default one, so the tap can name itself with no argument and no change to
# whatever statusline forked it. An explicit $1 still wins, for a statusline
# that would rather be told.
#
# This must produce the same string as profile_name_of in limits-poller.sh and
# ProfileName in internal/installer/settings.go: the poller reads the file this
# writes by that name, and the dashboard joins it to the OTel profile label.
profile_name() {
  local d="${1:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}}" base
  if [ -f "$OBS_DIR/profiles.json" ]; then
    d=$(cd "$d" 2>/dev/null && pwd -P) || return 1
    /usr/bin/jq -r --arg directory "$d" '.profiles[] | select(.provider == "claude" and .enabled and .directory == $directory) | .id' "$OBS_DIR/profiles.json" 2>/dev/null
    return
  fi
  base=$(basename "${d%/}")
  base=${base#.claude-}
  base=${base#.claude}
  [ -z "$base" ] && base=default
  printf '%s' "$base"
}
PROFILE=$(profile_name "${1:-}")
[ -n "$PROFILE" ] || exit 0

input=$(cat)

# Renders without rate_limits happen in production too (the field is absent
# until the session's first API response); skip the write so that null never
# masks real window data already on disk.
printf '%s' "$input" | /usr/bin/jq -e '.rate_limits != null' >/dev/null 2>&1 || exit 0

tmp="$STATE/.tap.$$"
printf '%s' "$input" | /usr/bin/jq -c '{ts_ms: (now*1000|floor), rate_limits: (.rate_limits // null),
                 session_id: (.session_id // null), cost: (.cost // null)}' \
  > "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 0; }

# Per profile, because two profiles rendering into one file would each
# overwrite the other's rate limits every second, which is indistinguishable
# from one profile whose numbers jump. The poller reads the same name, and
# both derive it the same way, so no legacy file is needed here: this script
# and the poller are always replaced together by one install.
mv "$tmp" "$STATE/statusline-latest.$PROFILE.json"
