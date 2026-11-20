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

# One jq pass, printing three lines: the record to publish, the session id
# (empty when absent or not a plain id, since it becomes a file name), and
# what this session last heard from the API: its figures and its running API
# time. Renders without rate_limits happen in production too (the field is
# absent until the session's first API response); they print nothing and
# write nothing, so that null never masks real window data already on disk.
parsed=$(printf '%s' "$input" | /usr/bin/jq -r 'select(.rate_limits != null) |
  ({ts_ms: (now*1000|floor), rate_limits, session_id: (.session_id // null), cost: (.cost // null)} | tojson),
  (.session_id // "" | tostring | if test("^[A-Za-z0-9_-]+$") then . else "" end),
  ({rate_limits, api_ms: (.cost.total_api_duration_ms // null)} | tojson)' 2>/dev/null) || exit 0
[ -n "$parsed" ] || exit 0
{ IFS= read -r record; IFS= read -r session; IFS= read -r heard; } <<<"$parsed"

LATEST="$STATE/statusline-latest.$PROFILE.json"

# Every open session of a profile renders into that one file, and each render
# carries the figures its session last heard from the API, however long ago.
# An idle session therefore holds whatever it heard last: measured 2026-11-20,
# a session idle since just before an early weekly reset kept rendering 98%
# while its account stood at 20%, and each time it redrew, the dashboard
# showed 98% for minutes at a time. So a session publishes only when its
# figures or its running API time moved since its own previous render, which
# means it heard from the API in between, or when the file is already its own,
# which keeps the active session's file fresh between replies. A session seen
# for the first time cannot say how old its figures are, so it only records
# them. A render with no session id cannot be told apart from any other and
# publishes as it always has.
if [ -n "$session" ]; then
  SEEN="$STATE/statusline-seen"
  mark="$SEEN/$PROFILE.$session"
  previous=$(cat "$mark" 2>/dev/null)
  publish=0
  if [ "$heard" != "$previous" ]; then
    [ -n "$previous" ] && publish=1
    mkdir -p "$SEEN" 2>/dev/null
    { printf '%s' "$heard" > "$SEEN/.$$" && mv "$SEEN/.$$" "$mark"; } 2>/dev/null || rm -f "$SEEN/.$$"
    # A mark untouched for a week goes. Its session, if it renders again, is a
    # first sighting, and a first sighting never publishes.
    find "$SEEN" -type f -mtime +7 -delete 2>/dev/null
  fi
  if [ "$publish" = 0 ]; then
    [ "$(/usr/bin/jq -r '.session_id // empty' "$LATEST" 2>/dev/null)" = "$session" ] || exit 0
  fi
fi

tmp="$STATE/.tap.$$"
printf '%s\n' "$record" > "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 0; }

# Per profile, because two profiles rendering into one file would each
# overwrite the other's rate limits every second, which is indistinguishable
# from one profile whose numbers jump. The poller reads the same name, and
# both derive it the same way, so no legacy file is needed here: this script
# and the poller are always replaced together by one install.
mv "$tmp" "$LATEST"
