#!/bin/bash
# Merges .claude.json cache + statusline tap into VM metrics + statusline segment.
set -u
# Refuse the source checkout: this script writes state/statusline-segment.txt
# and appends to logs/poller.err beside itself, and in the checkout that means
# inside the repository. go.mod is tracked there and never copied to an
# install dir. See bin/fetch-stores.sh for the full reasoning.
if [ -f "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/go.mod" ]; then
    echo "error: refusing to run from the source checkout." >&2
    echo "       these scripts run from an install dir; see: claude-obs install" >&2
    exit 1
fi
# shellcheck disable=SC1091
# ^ config.env is generated during install; this is the expected execution path
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/config.env"

# The default profile keeps its config at ~/.claude.json, NOT at
# $PROFILE_DIR/.claude.json. Claude Code only moves that file inside the
# profile directory when CLAUDE_CONFIG_DIR names one; with the variable unset
# it stays in $HOME and ~/.claude holds only the rest of the profile. Resolving
# it the uniform way pointed the poller at ~/.claude/.claude.json, which does
# not exist, so an install against the shipped default of $HOME/.claude emitted
# no plan-window metrics at all and said nothing about why.
#
# is_default_profile is also what decides whether the probe below sets
# CLAUDE_CONFIG_DIR: exporting it as $HOME/.claude is NOT the same as leaving
# it unset, since it sends Claude Code to that second config file rather than
# the one the default profile actually uses.
is_default_profile() { [ "${1%/}" = "${HOME%/}/.claude" ]; }

cache_path_for() { # $1 profile dir -> the config file Claude Code actually uses
  if is_default_profile "$1"; then printf '%s' "$HOME/.claude.json"
  else printf '%s' "${1%/}/.claude.json"; fi
}

# profile_name_of must agree with internal/installer/settings.go's ProfileName,
# because that is what stamps profile=<name> on the OTel resource attributes.
# The two names meeting is the entire reason a dashboard can put a session's
# cost next to that session's plan window.
profile_name_of() {
  local base; base=$(basename "${1%/}")
  base=${base#.claude-}
  base=${base#.claude}
  [ -z "$base" ] && base=default
  printf '%s' "$base"
}

# A plan window belongs to an ACCOUNT, not to a profile, and the two are not
# one to one: several profile directories can be signed in to the same Claude
# account, in which case they share one allowance. Labelling only by profile
# would draw one meter per profile and read as that many separate allowances.
# account_of is the join key; claude_profile_account_info carries the readable
# name so nothing downstream has to parse a uuid.
account_of() { jq -r '.oauthAccount.accountUuid // "unknown" | .[0:8]' "$1" 2>/dev/null || echo unknown; }
domain_of()  { jq -r '.oauthAccount.emailAddress // "unknown" | split("@") | last' "$1" 2>/dev/null || echo unknown; }

# PROFILE_DIRS is the list; PROFILE_DIR remains the single-profile spelling, so
# an install that predates this key keeps working untouched.
IFS=':' read -r -a PROFILE_LIST <<< "${PROFILE_DIRS-${PROFILE_DIR-}}"
IFS=':' read -r -a PROFILE_ID_LIST <<< "${PROFILE_IDS-}"

POLLER_DRY_RUN="${POLLER_DRY_RUN:-0}"
now_ms=$(($(date +%s) * 1000)); lines=""

# ---- refresh the billed figure ----------------------------------------------
# The real dollars only change when Claude Code fetches GET /api/oauth/usage,
# and until now only running /usage in a session did that: the dashboard could
# show a figure hours old, one Claude Code itself would have discarded (it
# treats its own cache as dead after an hour). The control protocol exposes
# that fetch as a get_usage request, which a headless process answers, so the
# poller asks for it here.
#
# Measured on 2.1.227: 2.7s, no model tokens, no transcript written. The probe
# does not read the response: the durable effect is that Claude Code rewrites
# cachedUsageUtilization, which the block below reads as it always has, so
# there is still exactly one source for the billed figure and the "as of"
# age stays honest when a probe fails. Claude Code throttles that rewrite to
# once every 5 minutes, hence the default MIN_AGE: probing more often than the
# data can change is just spawned processes.
#
# --settings is not decoration. Without it the probe counts itself as a
# session in claude_code.session.count, roughly 288 phantom sessions a day at
# this cadence, and the plain CLAUDE_CODE_ENABLE_TELEMETRY env var does not
# work because the profile's settings.json wins over the environment.
PROBE="${USAGE_PROBE:-1}"
PROBE_MIN_AGE_S="${USAGE_PROBE_MIN_AGE_S:-300}"
PROBE_TIMEOUT_S="${USAGE_PROBE_TIMEOUT_S:-30}"
PROBE_SETTINGS='{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}'
PROBE_REQUEST='{"type":"control_request","request_id":"poller","request":{"subtype":"get_usage"}}'

cache_fetched_at() { jq -r '.cachedUsageUtilization.fetchedAtMs // 0' "$1" 2>/dev/null || echo 0; }

# Spawned in the background with a watchdog: the process exits on its own when
# stdin closes, so the kill is a backstop against a hung one blocking a poller
# run (launchd starts another every 60s regardless).
#
# $1 is the profile dir being probed. $2 is where to keep the response.
#
# The response is kept for exactly one field, rate_limits_available, and for
# nothing else. Every NUMBER still comes from cachedUsageUtilization afterwards,
# because the response's own figures are not always populated (a rapid repeat
# comes back seeded from headers) and one source is what keeps the "as of"
# caption honest. But the cache cannot answer "is there anything to fetch at
# all": a profile whose credentials have lapsed leaves cachedUsageUtilization
# absent forever, which is indistinguishable from a profile that simply has not
# been probed yet. Claude Code answers that question directly and only here.
run_probe() {
  local pid killer PROFILE_DIR="$1" out="$2"
  # CLAUDE_CONFIG_DIR is set for a named profile and deliberately left unset
  # for the default one: see is_default_profile above. Setting it to
  # $HOME/.claude would send the probe at a different config file from the one
  # this poller reads, so the probe would refresh a cache nothing looks at.
  if is_default_profile "$PROFILE_DIR"; then
    printf '%s\n' "$PROBE_REQUEST" \
      | "$CLAUDE_BIN" \
          --input-format stream-json --output-format stream-json --verbose -p \
          --settings "$PROBE_SETTINGS" >"$out" 2>>"$OBS_DIR/logs/poller.err" &
  else
    printf '%s\n' "$PROBE_REQUEST" \
      | CLAUDE_CONFIG_DIR="$PROFILE_DIR" "$CLAUDE_BIN" \
          --input-format stream-json --output-format stream-json --verbose -p \
          --settings "$PROBE_SETTINGS" >"$out" 2>>"$OBS_DIR/logs/poller.err" &
  fi
  pid=$!
  ( sleep "$PROBE_TIMEOUT_S"; kill -TERM "$pid" 2>/dev/null ) >/dev/null 2>&1 &
  killer=$!
  wait "$pid" 2>/dev/null
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
}

# poll_profile emits one profile's metrics into the global $lines.
#
# Every series it writes carries profile= and account=. The profile label is
# the same string internal/installer/settings.go stamps onto the OTel resource
# attributes, so a session's cost and that session's plan window can be joined.
# The account label exists because the allowance is the account's, not the
# profile's: two profile directories signed in to one account share one set of
# windows, and labelling only by profile would present that as two allowances.
poll_profile() {
  local PROFILE_DIR="$1"
  local CACHE TAP profile account domain LBL plines
  local probe_ran=0 probe_moved=0 before_fetch after_fetch probe_out usage_avail=""
  local CONFIRMED confirmed_at confirmed_age
  local cache_ok=1 u fetched cache_age tap_age tts best in_over found_pct usd seg

  profile="${2:-$(profile_name_of "$PROFILE_DIR")}"
  CACHE=$(cache_path_for "$PROFILE_DIR")
  [ -n "${POLLER_CACHE_FILE:-}" ] && CACHE="$POLLER_CACHE_FILE"
  TAP="${POLLER_TAP_FILE:-$OBS_DIR/state/statusline-latest.$profile.json}"
  CONFIRMED="$OBS_DIR/state/usage-confirmed.$profile"
  account=$(account_of "$CACHE")
  domain=$(domain_of "$CACHE")
  LBL="profile=\"$profile\",account=\"$account\""
  plines=""

  if [ ! -f "$CACHE" ]; then
    cache_ok=0
    echo "warning: no Claude Code config at $CACHE (profile $profile)." >&2
    echo "         plan-window and billed-figure metrics cannot be emitted." >&2
  elif ! jq -e '.cachedUsageUtilization' "$CACHE" >/dev/null 2>&1; then
    # Normal on a profile that has never fetched /usage. The probe below is
    # what populates it, so this clears itself on a working install.
    cache_ok=0
  fi

  before_fetch=$(cache_fetched_at "$CACHE")
  [[ "$before_fetch" =~ ^[0-9]+$ ]] || before_fetch=0
  if [ "$PROBE" = 1 ] && [ "$POLLER_DRY_RUN" != 1 ] && [ -x "$CLAUDE_BIN" ] \
     && [ -f "$CACHE" ] \
     && [ $(( (now_ms - before_fetch) / 1000 )) -ge "$PROBE_MIN_AGE_S" ]; then
    probe_ran=1
    probe_out="$OBS_DIR/state/.probe.$profile.$$"
    run_probe "$PROFILE_DIR" "$probe_out"
    # An unparseable or absent response leaves usage_avail empty, and an empty
    # value emits no metric at all: the same "blank is not zero" rule the
    # delivery counters follow. A probe that could not be read must not be
    # recorded as a profile that cannot report.
    usage_avail=$(jq -r 'select(.type=="control_response")
                         | .response.response.rate_limits_available
                         | if . == true then 1 elif . == false then 0 else empty end' \
                    "$probe_out" 2>/dev/null | head -1)
    rm -f "$probe_out"
    # Stamp the confirmation. fetchedAtMs answers "when did this value last
    # CHANGE", because Claude Code does not rewrite cachedUsageUtilization
    # when a fetch comes back identical, so a profile sitting at a steady
    # percentage ages indefinitely while its numbers are correct to the
    # second. Measured 2026-09-21: a profile read 44 minutes stale against a
    # live probe and a 6 second old statusline tap that both agreed with it to
    # the point. This file answers the other question, "when did we last CHECK",
    # and only a live fetch sets it: the cache merely holding a value is not
    # evidence that anyone confirmed it.
    #
    # Only a probe that came back with usable rate limits counts. A probe
    # answering rate_limits_available=false (a profile whose sign-in has
    # lapsed) has confirmed that there is nothing to fetch, not that the
    # figures are current, so it must not stamp.
    [ "$usage_avail" = 1 ] && printf '%s' "$(date +%s)" > "$CONFIRMED.tmp" \
      && mv "$CONFIRMED.tmp" "$CONFIRMED"
    after_fetch=$(cache_fetched_at "$CACHE")
    [[ "$after_fetch" =~ ^[0-9]+$ ]] || after_fetch=0
    [ "$after_fetch" -gt "$before_fetch" ] && probe_moved=1
    # The probe takes a couple of seconds and the cache it just refreshed is
    # stamped at the end of them, so every age below has to be measured from
    # after it. Reusing the start-of-run clock reported the freshest possible
    # data as a negative number of seconds old.
    now_ms=$(($(date +%s) * 1000))
  fi

  u=$(jq -c '.cachedUsageUtilization // empty' "$CACHE" 2>/dev/null)
  cache_age=999999
  if [ -n "$u" ]; then
    fetched=$(jq -r '.fetchedAtMs // 0' <<<"$u")
    [[ "$fetched" =~ ^[0-9]+$ ]] || fetched=0
    cache_age=$(( (now_ms - fetched) / 1000 ))
    while IFS=$'\t' read -r kind pct reset; do
      [ "$pct" = null ] && continue
      win=$kind; [ "$kind" = weekly_all ] && win=seven_day; [ "$kind" = session ] && win=five_hour
      plines+="claude_plan_window_pct{window=\"$win\",source=\"cache\",$LBL} $pct
"
      if [ "$reset" != null ] && [ -n "$reset" ]; then
        # resets_at arrives as UTC (+00:00); parse under TZ=UTC, not local time
        rs=$(TZ=UTC date -j -f '%Y-%m-%dT%H:%M:%S' "${reset%%.*}" +%s 2>/dev/null || echo 0)
        # Round the sub-second part rather than letting the truncation above
        # stand. The same reset instant comes back either side of the whole
        # second on consecutive refetches (02:59:59.574463 and 03:00:00.410182
        # were both observed for one boundary), and truncating turned that into
        # an epoch that alternated by one. usage-alert.sh keys its dedup on this
        # value, so the wobble read as a fresh reset period and re-fired every
        # crossed threshold every few minutes. Rounding usually lands on the
        # whole second the statusline tap reports, but it does not cure the
        # wobble and must not be relied on to: the observed spread runs from
        # 02:59:59.52 to past 03:00:00.6, so it straddles the .5 boundary too and
        # still alternates by one, just less often. What actually holds the
        # alerts steady is same_marker's slack in usage-alert.sh.
        frac=${reset#*.}
        [ "$rs" != 0 ] && [ "$frac" != "$reset" ] && [ "${frac:0:1}" -ge 5 ] 2>/dev/null \
          && rs=$((rs + 1))
        [ "$rs" != 0 ] && plines+="claude_plan_window_reset_timestamp_seconds{window=\"$win\",source=\"cache\",$LBL} $rs
"
      fi
    done < <(jq -r '.utilization.limits[]? | [.kind, (.percent // "null"), (.resets_at // "null")] | @tsv' <<<"$u")
    used=$(jq -r '.utilization.extra_usage.used_credits // empty' <<<"$u")
    [[ "$used" =~ ^[0-9]+$ ]] && plines+="claude_extra_usage_usd{$LBL} $(echo "scale=2; $used/100" | bc)
"
    plines+="claude_extra_usage_enabled{$LBL} $(jq -r 'if .utilization.extra_usage.is_enabled then 1 else 0 end' <<<"$u")
claude_spend_limit_reached{$LBL} $(jq -r 'if .utilization.extra_usage.spend_limit_reached then 1 else 0 end' <<<"$u")
claude_plan_data_age_seconds{source=\"cache\",$LBL} $cache_age
"
  fi

  # How long since a live fetch last confirmed this profile's figures, as
  # against how long since they last changed (claude_plan_data_age_seconds
  # above). Emitted on every run, including runs that did not probe, because
  # the answer is "N seconds since the last confirmation" either way and a
  # gap in the series would read as a poller that had stopped.
  #
  # Absent, never zero and never a sentinel, until something has actually
  # confirmed: no stamp means no probe has ever come back with usable rate
  # limits for this profile, which is true of a signed-out profile and of an
  # install running with USAGE_PROBE=0. A reader must say "not confirmed"
  # rather than "confirmed at the epoch".
  if [ -f "$CONFIRMED" ]; then
    confirmed_at=$(cat "$CONFIRMED" 2>/dev/null)
    if [[ "$confirmed_at" =~ ^[0-9]+$ ]]; then
      confirmed_age=$(( (now_ms / 1000) - confirmed_at ))
      [ "$confirmed_age" -lt 0 ] && confirmed_age=0
      plines+="claude_plan_data_confirmed_age_seconds{source=\"cache\",$LBL} $confirmed_age
"
    fi
  fi

  tap_age=999999
  if [ -f "$TAP" ]; then
    tts=$(jq -r '.ts_ms // 0' "$TAP")
    [[ "$tts" =~ ^[0-9]+$ ]] || tts=0
    tap_age=$(( (now_ms - tts) / 1000 ))
    for w in five_hour seven_day; do
      pv=$(jq -r ".rate_limits.$w.used_percentage // empty" "$TAP")
      [ -n "$pv" ] && plines+="claude_plan_window_pct{window=\"$w\",source=\"statusline\",$LBL} $pv
"
      # The tap carries the reset time too, already as epoch seconds (the cache
      # spells it as an ISO string, hence the parsing above). It used to be
      # dropped, which left the cache as the only writer of a reset time: with
      # that cache 6.8 hours stale it reported a five hour window that had ended
      # 113 minutes earlier, and the dashboard faithfully rendered "resetting
      # now" for as long as it took Claude Code to refetch /usage.
      r=$(jq -r ".rate_limits.$w.resets_at // empty" "$TAP")
      [[ "$r" =~ ^[0-9]+$ ]] && plines+="claude_plan_window_reset_timestamp_seconds{window=\"$w\",source=\"statusline\",$LBL} $r
"
    done
    plines+="claude_plan_data_age_seconds{source=\"statusline\",$LBL} $tap_age
"
  fi

  # get_pct pulls one window/source percent back out of this profile's own
  # accumulated text. NOTE: it re-parses the exact line format emitted above;
  # keep the two in sync if that format ever changes.
  get_pct() {
    printf '%s' "$plines" | awk -F'[ }]' -v pat="window=\"$1\",source=\"$2\"" '$0 ~ pat {print $(NF)}' | head -1
  }

  # in_overage: five_hour/seven_day read from whichever of cache/statusline is
  # fresher (the same "best" source the segment's headline percent uses below);
  # weekly_scoped only ever exists in the cache poll, so it always reads from
  # cache regardless of which source is freshest. 1 if any of those three
  # window values is >= 100.
  best=$( [ "$tap_age" -lt "$cache_age" ] && echo statusline || echo cache )
  in_over=0
  found_pct=0
  for v in "$(get_pct five_hour "$best")" "$(get_pct seven_day "$best")" "$(get_pct weekly_scoped cache)"; do
    if [ -n "$v" ]; then
      found_pct=1
      [ "${v%.*}" -ge 100 ] 2>/dev/null && in_over=1
    fi
  done
  # Only emit claude_in_overage when at least one window percentage was actually
  # found this run; with both cache and tap absent, a lone "0" reads as a
  # false-confident "not in overage" rather than the honest "no data".
  [ "$found_pct" = 1 ] && plines+="claude_in_overage{$LBL} $in_over
"

  # Whether the refresh above actually moved the billed figure, so the Health
  # page can tell "nothing has changed" from "the refresh is broken". Only
  # emitted on runs that probed: a run that skipped because the cache was still
  # fresh has nothing to report and must not look like a failure.
  [ "$probe_ran" = 1 ] && plines+="claude_usage_probe_ok{profile=\"$profile\"} $probe_moved
"

  # Whether this profile can fetch usage AT ALL, which is a different question
  # from whether the last fetch moved anything. Claude Code answers 0 for a
  # profile whose credentials have lapsed: it reports no subscription and no
  # rate limits, so cachedUsageUtilization will never appear and
  # claude_poller_cache_ok stays 0 forever while being entirely correct. Without
  # this line the dashboard can only show that permanent zero, which reads as a
  # broken poller rather than a profile nobody is signed in to.
  [ -n "$usage_avail" ] && plines+="claude_profile_usage_available{$LBL} $usage_avail
"

  # Always emitted, including on a run that found nothing else to say: this is
  # the one line that distinguishes a healthy quiet poller from a misconfigured
  # one. Placed after the probe so it reflects the cache as it stands at the end
  # of the run, not as it was before the probe refreshed it.
  if [ "$cache_ok" = 0 ] && [ -f "$CACHE" ] && jq -e '.cachedUsageUtilization' "$CACHE" >/dev/null 2>&1; then
    cache_ok=1
  fi
  plines+="claude_poller_cache_ok{profile=\"$profile\"} $cache_ok
"

  # The profile -> account mapping, as a standard *_info gauge: value 1, the
  # facts in the labels. It is what lets the dashboard title an account and say
  # which profiles feed it without parsing a uuid or guessing from a domain.
  plines+="claude_profile_account_info{profile=\"$profile\",account=\"$account\",domain=\"$domain\"} 1
"

  # Pre-render this profile's statusline MTD overage tail (the statusline
  # script just cats the file). Live wk/5h percentages render from statusline
  # stdin now, so this segment carries only what stdin cannot know: billed
  # overage dollars from the cache. No (stale) marker here: the usage cache
  # refreshes so rarely (often a day between fetches) that the marker would be
  # lit permanently; cache age is still exported above as
  # claude_plan_data_age_seconds{source="cache"}.
  usd=$(printf '%s' "$plines" | awk '/^claude_extra_usage_usd/ {print $2}')
  seg=""
  if [ "$in_over" = 1 ] && [ -n "${usd:-}" ]; then
    seg=$(printf '\033[2m|\033[0m \033[1;31mreported overage $%s MTD\033[0m' "$usd")
  fi
  if [ "$POLLER_DRY_RUN" != 1 ]; then
    write_segment "$OBS_DIR/state/statusline-segment.$profile.txt" "$seg"
    # The unsuffixed file is what a statusline wired before this existed still
    # reads, so the profile the install was originally pointed at keeps working
    # with no change on the statusline side.
    [ "$profile" = "$LEGACY_PROFILE" ] && write_segment "$OBS_DIR/state/statusline-segment.txt" "$seg"
  fi

  lines+="$plines"
}

write_segment() { printf '%s' "$2" > "$1.tmp" && mv "$1.tmp" "$1"; }

LEGACY_PROFILE="${PROFILE_ID_LIST[0]:-$(profile_name_of "${PROFILE_DIR-}")}"

for _index in "${!PROFILE_LIST[@]}"; do
  _pd="${PROFILE_LIST[$_index]}"
  [ -n "$_pd" ] || continue
  poll_profile "$_pd" "${PROFILE_ID_LIST[$_index]-}"
done

if [ "$POLLER_DRY_RUN" = 1 ]; then
  printf '%s' "$lines"
else
  printf '%s' "$lines" | curl -fsS --max-time 5 --data-binary @- \
    "$VM_URL/api/v1/import/prometheus" 2>>"$OBS_DIR/logs/poller.err" || true
fi
