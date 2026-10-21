#!/bin/bash
# Threshold alerts, edge-triggered. DRY_RUN=1 prints instead of notifying.
set -u
# Refuse the source checkout: this script creates state/alerts-state.json
# beside itself, and in the checkout that means inside the repository. go.mod
# is tracked there and never copied to an install dir. See bin/fetch-stores.sh
# for the full reasoning.
if [ -f "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/go.mod" ]; then
    echo "error: refusing to run from the source checkout." >&2
    echo "       these scripts run from an install dir; see: claude-obs install" >&2
    exit 1
fi
# shellcheck disable=SC1091
# ^ config.env is generated during install; this is the expected execution path
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/config.env"
USAGE_STALE_ALERT_S="${USAGE_STALE_ALERT_S:-21600}" # old config.env files predate this key
NOTIFIER_APP="${NOTIFIER_APP:-$HOME/Applications/Agent Observability.app}" # ditto
NOTIFIER_BIN="$NOTIFIER_APP/Contents/MacOS/AgentObsNotifier"
# localhost, not the loopback IP: the analysis runner's CORS allowlist is exactly
# http://localhost:$DASH_PORT and the Sessions tab breaks from the other form.
DASH_URL="http://localhost:${DASH_PORT:-8427}"
STATE="$OBS_DIR/state/alerts-state.json"; [ -f "$STATE" ] || echo '{}' > "$STATE"

qraw() { curl -fsS --max-time 5 "$VM_URL/api/v1/query" --data-urlencode "query=$1"; }

# Both plan-window series are written twice, once per source, so a window has to
# be resolved to one number before it can be compared against a threshold.
# max() cannot do that: it answers "which is larger" rather than "which is
# true", and the larger number is routinely the staler one. Measured here with
# the tap 9 hours cold: it still read five_hour at 14% against a reset that had
# passed 5.5 hours earlier, while the freshly probed cache read 0%. Left on
# max(), a tap that freezes above a threshold keeps firing that threshold for a
# window that has since reset to empty. Freshness is the only defensible
# tie-break. pick() is internal/plan's PickFreshest in shell, down to ranking
# a source with no age at all last rather than trusting it over one with a
# known age; the two implementations must agree.
pick() { # $1 JSON  $2 account  $3 window -> freshest matching profile/source
  jq -rn --argjson m "$1" --argjson a "$OBSERVED_JSON" --arg account "$2" --arg w "$3" '
    def key: [.metric.account, .metric.profile, .metric.source] | tojson;
    (($a.data.result // []) | map({(key): (.value[1] | tonumber? // null)}) | add // {}) as $observed
    | ($m.data.result // [])
    | map(select(.metric.account == $account and .metric.window == $w))
    | map(select((.value[1] | tonumber?) != null))
    | map({v: (.value[1] | tonumber), s: (.metric.source // ""), p: (.metric.profile // ""), r: -($observed[key] // 0)})
    | sort_by(.r, .s, .p)
    | if length == 0 then empty else .[0].v end'
}

# A reset time is a dedup marker, and same_marker compares markers as whole
# numbers. VictoriaMetrics sends plain integers, but jq keeps the notation it
# was given, so a value that arrived as 1.79020455e+09 would print as
# 1.79020455E+9: not a whole number, never the same period as the next reading,
# and "date -r 1" once truncated. Normalise it before it is used either way.
epoch() { # $1 number in any notation -> whole seconds, or nothing
  [ -n "$1" ] && awk -v v="$1" 'BEGIN { printf "%.0f", v }'
}

# Scalar readings are selected within one account, using the matching cache
# observation. Profiles on the same account share one bill; never add them.
account_value() { # $1 JSON  $2 account
  jq -rn --argjson m "$1" --argjson a "$OBSERVED_JSON" --arg account "$2" '
    (($a.data.result // []) | map(select(.metric.account == $account and .metric.source == "cache") | {(.metric.profile // ""): (.value[1] | tonumber? // 0)}) | add // {}) as $observed
    | ($m.data.result // []) | map(select(.metric.account == $account))
    | map(select((.value[1] | tonumber?) != null))
    | sort_by(-($observed[.metric.profile // ""] // 0), .metric.profile)
    | if length == 0 then empty else .[0].value[1] | tonumber end'
}

account_name() {
  jq -rn --arg account "$1" --argjson p "$PCT_JSON" --argjson a "$OBSERVED_JSON" --argjson registry "$REGISTRY_JSON" '
    [($p.data.result // [])[], ($a.data.result // [])[]] | map(select(.metric.account == $account) | .metric.profile // empty) | unique as $ids
    | [$ids[] as $id | ([$registry[] | select(.provider == "claude" and .id == $id) | .name | select(length > 0)][0] // $id)]
    | map(select(length > 0)) | unique | if length > 0 then join(" / ") else "Account " + $account end'
}

window_label() {
  case "$1" in
    five_hour) echo '5-hour allowance' ;;
    seven_day) echo 'Weekly allowance' ;;
    weekly_scoped) echo 'Model weekly allowance' ;;
    *) echo "$1" ;;
  esac
}

# Fetched once per run and resolved per window below, rather than re-queried
# for every window and threshold.
OBSERVED_JSON=$(qraw 'timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds' || echo '{}')
PCT_JSON=$(qraw 'claude_plan_window_pct' || echo '{}')
RESET_JSON=$(qraw 'claude_plan_window_reset_timestamp_seconds' || echo '{}')
BURN_JSON=$(qraw 'clamp_min(delta(claude_extra_usage_usd{account!=""}[1h]),0)' || echo '{}')
REGISTRY_JSON=$(jq -c '.profiles // []' "$OBS_DIR/profiles.json" 2>/dev/null || echo '[]')

# Unlabelled pre-multi-account metrics can be historical evidence, but cannot
# identify the current account for an alert. Failed queries leave state alone.
ACCOUNTS=$(jq -rn --argjson p "$PCT_JSON" --argjson a "$OBSERVED_JSON" --argjson b "$BURN_JSON" '
  [($p.data.result // [])[], ($a.data.result // [])[], ($b.data.result // [])[]]
  | map(.metric.account // empty) | map(select(length > 0 and . != "unknown")) | unique[]')

# Reset-time markers can wobble a second or two between refetches without the
# reset period having changed; bin/limits-poller.sh rounds that away at source,
# and this is the backstop for any residual drift. Two numeric markers within
# MARKER_SLACK_S of each other are the same period. The closest two real
# periods ever get is the five hour window, so this can only ever swallow a
# repeat of an alert already delivered, never a genuine new window. Markers
# that are not plain numbers ("high", "stale", a YYYY-MM-DD date) still have to
# match exactly.
MARKER_SLACK_S="${MARKER_SLACK_S:-120}"
same_marker() { # $1 previous marker  $2 new marker
  [ "$1" = "$2" ] && return 0
  [[ "$1" =~ ^[0-9]+$ && "$2" =~ ^[0-9]+$ ]] || return 1
  local d=$(( $1 - $2 )); [ "$d" -lt 0 ] && d=$(( -d ))
  [ "$d" -le "$MARKER_SLACK_S" ]
}

notify() { # $1 key  $2 marker  $3 title  $4 body  $5 dashboard fragment
  local prev; prev=$(jq -r --arg k "$1" '.[$k] // ""' "$STATE")
  same_marker "$prev" "$2" && return 0
  # Missing reset metadata cannot establish a new period. When a formerly
  # unknown reset becomes known, remember it without repeating the warning.
  if [ -n "$prev" ] && { [ "$2" = na ] || [ "$prev" = na ]; }; then
    if [ "$2" != na ]; then
      jq --arg k "$1" --arg v "$2" '.[$k]=$v' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"
    fi
    return 0
  fi
  # Three tiers. The notifier app posts a banner that opens the dashboard when
  # clicked; if it is absent, unauthorized, or fails for any reason we fall back
  # to the plain osascript banner. An alert is never lost, only its click target.
  if [ "$DRY_RUN" = 1 ]; then
    echo "ALERT [$3] $4 -> $DASH_URL$5"
  elif [ -x "$NOTIFIER_BIN" ] && "$NOTIFIER_BIN" "$3" "$4" "$DASH_URL$5" 2>/dev/null; then
    :
  else
    # Account names are data, never interpolated into executable AppleScript.
    if ! osascript -e 'on run argv' \
      -e 'display notification (item 2 of argv) with title (item 1 of argv) sound name "Sosumi"' \
      -e 'end run' -- "$3" "$4"; then
      echo "warning: notification delivery failed for $1" >&2
      return 1
    fi
  fi
  jq --arg k "$1" --arg v "$2" '.[$k]=$v' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"
}
clear_key() { jq --arg k "$1" 'del(.[$k])' "$STATE" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"; }

age_human() { # $1 seconds -> human age, e.g. 45s / 12m / 6.5h / 2.1d
  awk -v s="$1" 'BEGIN {
    if (s < 90) printf "%.0fs", s
    else if (s < 5400) printf "%.0fm", s / 60
    else if (s < 172800) printf "%.1fh", s / 3600
    else printf "%.1fd", s / 86400
  }'
}

# Each account owns its readings, deep links and edge-trigger state. A warning
# on one account must neither describe nor suppress a warning on another.
while IFS= read -r account; do
  [ -n "$account" ] || continue
  name=$(account_name "$account")
  # Match db.AccountKey's component QueryEscape, then escape the whole key
  # for the query string. Spaces in Go's inner query encoding are plus signs.
  account_key="claude:$(jq -rn --arg account "$account" '$account | @uri | gsub("%20"; "+")')"
  account_url="/?provider=claude&accountKey=$(jq -rn --arg key "$account_key" '$key | @uri')"
  state_prefix="claude.$account"
  io=''

# 1. Plan-window crossings (once per account/threshold/reset period)
for w in five_hour seven_day weekly_scoped; do
  pct=$(pick "$PCT_JSON" "$account" "$w"); [ -z "$pct" ] && continue
  if [ "${pct%.*}" -ge 100 ] 2>/dev/null; then io=1; elif [ "$io" != 1 ]; then io=0; fi
  reset=$(epoch "$(pick "$RESET_JSON" "$account" "$w")")
  label=$(window_label "$w")
  reset_text='Reset time unavailable.'
  if [ -n "$reset" ]; then
    reset_text="Resets $(/bin/date -r "${reset%.*}" '+%a %H:%M %Z')."
  fi
  for t in $WARN_PCTS; do
    if [ "${pct%.*}" -ge "$t" ] 2>/dev/null; then
      notify "$state_prefix.win.$w.$t" "${reset:-na}" "Claude · $name: ${pct%.*}% used" \
             "$label crossed ${t}%. $reset_text" "$account_url#now"
    elif [ "$(jq -r --arg k "$state_prefix.win.$w.$t" '.[$k] // ""' "$STATE")" = na ]; then
      clear_key "$state_prefix.win.$w.$t"
    fi
  done
done

# 2. Real overage burn (clamped: used_credits resets monthly)
# Re-arm rule: each threshold re-arms once burn falls below THAT threshold.
burn=$(account_value "$BURN_JSON" "$account")
if [ -n "$burn" ]; then
  b=${burn%.*}
  if [ "$b" -ge "$OVERAGE_CRIT_PER_HR" ]; then
    notify "$state_prefix.overage.crit" high "Claude · $name: overage critical" "Provider-reported overage rose by \$$b in the last hour." "$account_url#overage"
  elif [ "$b" -ge "$OVERAGE_WARN_PER_HR" ]; then
    notify "$state_prefix.overage.warn" high "Claude · $name: overage warning" "Provider-reported overage rose by \$$b in the last hour." "$account_url#overage"
    clear_key "$state_prefix.overage.crit"
  else clear_key "$state_prefix.overage.warn"; clear_key "$state_prefix.overage.crit"; fi
fi

# The former API-equivalent daily budget alert valued included plan usage.
# It is intentionally retired. Only provider-reported overage above produces
# monetary alerts; plan headroom is monitored with percentages.

# 4. Entered overage
# $io empty means the query failed (VM unreachable, e.g. right after a reboot):
# leave the dedup key untouched rather than treating a missing sample as "not
# in overage", or every outage re-arms the banner and it re-fires on recovery.
# Derived from the same per-window freshest readings above, rather than a
# profile-wide flag that may have been calculated from an older source.
if [ -n "$io" ]; then
  if [ "$io" = 1 ]; then
    reset=$(epoch "$(pick "$RESET_JSON" "$account" seven_day)")
    notify "$state_prefix.overage.entered" "${reset:-na}" "Claude · $name: allowance exhausted" \
           "A plan window hit 100%. Check paid-usage settings before continuing." \
           "$account_url#overage"
  else clear_key "$state_prefix.overage.entered"; fi
fi

# 5. Billed figure stale while an allowance is exhausted
# Same empty-means-query-failed guard as rule 4: an empty $io or $age leaves
# the dedup key untouched instead of treating a failed query as "not stale".
cache_json=$(jq -c '[.data.result[]? | select(.metric.source == "cache")] | {data: {result: .}}' <<<"$OBSERVED_JSON")
observed=$(account_value "$cache_json" "$account")
age=''
[ -n "$observed" ] && age=$(awk -v now="$(date +%s)" -v observed="$observed" 'BEGIN {printf "%.0f", now-observed}')
if [ -n "$io" ] && [ -n "$age" ]; then
  if [ "$io" = 1 ] && [ "${age%.*}" -gt "$USAGE_STALE_ALERT_S" ]; then
    notify "$state_prefix.usage_stale" stale "Claude · $name: billed figure stale" \
           "Reported overage figure is $(age_human "${age%.*}") stale while an allowance is exhausted. Run /usage to refresh it." \
           "$account_url#overage"
  else clear_key "$state_prefix.usage_stale"; fi
fi
done <<<"$ACCOUNTS"
