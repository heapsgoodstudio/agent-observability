#!/bin/bash
# Agent Observability status line: model | cwd (branch) | ctx % | 5h % . wk % | reported overage.
# Reads Claude Code's statusline JSON on stdin. Forks the tap so the poller
# sees live rate limits; appends the poller's pre-rendered MTD overage tail.
OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
input=$(cat)
printf '%s' "$input" | "$OBS_DIR/bin/statusline-tap.sh" >/dev/null 2>&1 &

# One jq pass; US-joined because read collapses empty tab-separated fields.
IFS=$'\x1f' read -r model dir ctx fivehour week < <(printf '%s' "$input" | /usr/bin/jq -r '
  [ (.model.display_name // ""),
    (.workspace.current_dir // .cwd // ""),
    (.context_window.used_percentage // "" | tostring),
    (.rate_limits.five_hour.used_percentage // "" | tostring),
    (.rate_limits.seven_day.used_percentage // "" | tostring)
  ] | join("\u001f")' 2>/dev/null)

sep=$'\033[2m|\033[0m'
dot=$'\033[2m\xc2\xb7\033[0m'
line=""
[ -n "$model" ] && line+="$(printf '\033[38;5;179m%s\033[0m' "$model")"
dirdisp=$dir
case "$dir" in "$HOME"*) dirdisp="~${dir#"$HOME"}" ;; esac
[ -n "$dir" ] && line+="${line:+ $sep }$(printf '\033[36m%s\033[0m' "$dirdisp")"
branch=""
[ -n "$dir" ] && branch=$(git -C "$dir" branch --show-current 2>/dev/null)
[ -n "$branch" ] && line+=" $(printf '\033[2m(%s)\033[0m' "$branch")"

# Thresholds: ctx 60/85, windows 75/90 (matching config.env alert thresholds).
pct_part() { # $1 label  $2 value  $3 yellow-at  $4 red-at
  local v=${2%.*} p
  p="$1 ${2%.*}%"
  if [ "$v" -ge "$4" ] 2>/dev/null; then p=$'\033[1;31m'"$p"$'\033[0m'
  elif [ "$v" -ge "$3" ] 2>/dev/null; then p=$'\033[33m'"$p"$'\033[0m'
  fi
  printf '%s' "$p"
}
group() { local out="" p; for p in "$@"; do out+="${out:+ $dot }$p"; done; printf '%s' "$out"; }

win_g=()
[ -n "$fivehour" ] && win_g+=("$(pct_part 5h "$fivehour" 75 90)")
[ -n "$week" ] && win_g+=("$(pct_part wk "$week" 75 90)")
[ -n "$ctx" ] && line+=" $sep $(pct_part ctx "$ctx" 60 85)"
[ ${#win_g[@]} -gt 0 ] && line+=" $sep $(group "${win_g[@]}")"

# Poller-rendered MTD overage tail (file carries its own leading pipe).
# Per profile, matching what the poller writes. CLAUDE_CONFIG_DIR is exported
# by Claude Code for a named profile and unset for the default one, which is
# the same derivation statusline-tap.sh and limits-poller.sh use. The
# unsuffixed file is the fallback, so a statusline still renders on an install
# whose poller has not yet written a per-profile segment.
_sl_profile() {
  local d="${CLAUDE_CONFIG_DIR:-$HOME/.claude}" base
  if [ -f "$OBS_DIR/profiles.json" ]; then
    d=$(cd "$d" 2>/dev/null && pwd -P) || return 1
    /usr/bin/jq -r --arg directory "$d" '.profiles[] | select(.provider == "claude" and .enabled and .directory == $directory) | .id' "$OBS_DIR/profiles.json" 2>/dev/null
    return
  fi
  base=$(basename "${d%/}"); base=${base#.claude-}; base=${base#.claude}
  [ -z "$base" ] && base=default
  printf '%s' "$base"
}
_profile=$(_sl_profile)
seg=""
if [ -n "$_profile" ]; then
  seg=$(cat "$OBS_DIR/state/statusline-segment.$_profile.txt" 2>/dev/null)
  if [ -z "$seg" ] && [ ! -f "$OBS_DIR/profiles.json" ]; then
    seg=$(cat "$OBS_DIR/state/statusline-segment.txt" 2>/dev/null)
  fi
fi
[ -n "$seg" ] && line+=" $seg"

printf '%s\n' "$line"
