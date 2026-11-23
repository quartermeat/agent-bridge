#!/data/data/com.termux/files/usr/bin/bash
# Shared by the two named hook entrypoints. All hook paths fail open.
set +e
SCRIPT=$1
PROVIDER=$2
GOJQ=${AGENT_BRIDGE_GOJQ:-$(command -v gojq)}
[[ -x $GOJQ ]] || exit 0
DIR=${AGENT_BRIDGE_ACTIVITY_DIR:-$HOME/.local/state/digital-terrarium/agents}

starttime() {
  local stat
  IFS= read -r stat < "/proc/$1/stat" || return 1
  stat=${stat##*) }
  set -- $stat
  printf '%s\n' "${20}"
}

host_pid() {
  local p=$PPID line args comm
  if [[ ${AGENT_BRIDGE_TEST_HOST_PID:-} =~ ^[0-9]+$ ]]; then
    printf '%s\n' "$AGENT_BRIDGE_TEST_HOST_PID"; return
  fi
  while [[ $p =~ ^[0-9]+$ && $p -gt 1 ]]; do
    args=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null)
    comm=$(cat "/proc/$p/comm" 2>/dev/null)
    if [[ $PROVIDER == claude ]]; then
      if [[ $args == *claude-termux* || $comm == claude ]]; then printf '%s\n' "$p"; return; fi
    elif [[ $comm == codex || $args == */bin/codex* ]]; then
      printf '%s\n' "$p"; return
    fi
    IFS= read -r line < "/proc/$p/stat" || break
    line=${line##*) }
    set -- $line
    p=$2
  done
}

write_record() {
  local path=$1 phase=$2 detail=$3 id=$4 session=$5 stamp epoch tmp
  stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  epoch=$(date +%s)
  tmp=$(mktemp "$DIR/.activity.XXXXXXXX") || return
  chmod 600 "$tmp"
  "$GOJQ" -n --arg id "$id" --arg name "${PROVIDER^}" --arg phase "$phase" --arg detail "$detail" --arg at "$stamp" --argjson epoch "$epoch" --arg session "$session" \
    '{id:$id,name:$name,phase:$phase,detail:$detail,report_origin:"event",sampledAt:$at,lastEventAt:$epoch,session_id:$session}' > "$tmp" && mv -f "$tmp" "$path"
  rm -f "$tmp"
}

watch() {
  local path=$1 host=$2 id=$3 base=${1%.json} initial current tmp stamp epoch quiet lockfd
  [[ $host =~ ^[0-9]+$ ]] || exit 0
  initial=$(starttime "$host") || exit 0
  quiet=${AGENT_BRIDGE_ACTIVITY_QUIET_SECONDS:-600}
  [[ $quiet =~ ^[0-9]+$ ]] || quiet=600
  exec {lockfd}> "$base.lock" || exit 0
  while :; do
    [[ $(starttime "$host") == "$initial" ]] || break
    flock -x "$lockfd" || break
    if [[ $(cat "$base.watch.pid" 2>/dev/null) != "$BASHPID" || ! -f $path ]]; then
      flock -u "$lockfd"
      break
    fi
    current=$("$GOJQ" -r --arg id "$id" 'if .id==$id then .phase else empty end' "$path" 2>/dev/null)
    case $current in
      thinking|working|tool) ;;
      idle|waiting|error) flock -u "$lockfd"; sleep 1; continue ;;
      *) flock -u "$lockfd"; break ;;
    esac
    stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    epoch=$(date +%s)
    tmp=$(mktemp "$DIR/.activity.XXXXXXXX") || { flock -u "$lockfd"; break; }
    chmod 600 "$tmp"
    "$GOJQ" --arg at "$stamp" --arg id "$id" --argjson now "$epoch" --argjson quiet "$quiet" \
      'if .id==$id and (.phase=="thinking" or .phase=="working" or .phase=="tool") then
        if ($now - (.lastEventAt // $now)) >= $quiet then
          .phase="waiting" | .detail="ready for direction" | .report_origin="watchdog" | .sampledAt=$at
        else .sampledAt=$at end
      else . end' "$path" > "$tmp" && mv -f "$tmp" "$path"
    rm -f "$tmp"
    flock -u "$lockfd"
    sleep 1
  done
  flock -x "$lockfd" || exit 0
  if [[ $(cat "$base.watch.pid" 2>/dev/null) == "$BASHPID" ]]; then
    rm -f "$base.watch.pid"
    [[ $(starttime "$host") == "$initial" ]] || rm -f "$path"
  fi
  flock -u "$lockfd"
}

main() {
  local payload event session id path host phase detail hash base wp pid cmd lockfd
  if [[ $3 == --watch ]]; then watch "$4" "$5" "$6"; return; fi
  payload=$(cat)
  event=$(printf '%s' "$payload" | "$GOJQ" -r '.hook_event_name // .event // .type // empty' 2>/dev/null)
  session=$(printf '%s' "$payload" | "$GOJQ" -r '.session_id // .sessionId // empty' 2>/dev/null)
  [[ -n $session && $session != null ]] || return
  if [[ $PROVIDER == claude ]]; then hash=$(printf 'claude:%s' "$session" | sha256sum); else hash=$(printf '%s' "$session" | sha256sum); fi
  hash=${hash:0:12}; id=$PROVIDER-$hash
  mkdir -p -m 700 "$DIR" || return
  chmod 700 "$DIR" 2>/dev/null
  path=$DIR/$id.json; base=${path%.json}; wp=$base.watch.pid
  exec {lockfd}> "$base.lock" || return
  flock -x "$lockfd" || return
  case $event in
    UserPromptSubmit|AfterAgentStart|TurnStart) phase=thinking; detail='processing request' ;;
    PreToolUse|PostToolUse) phase=tool; detail='using tool' ;;
    PermissionRequest|Notification) phase=waiting; detail='approval requested' ;;
    Stop|AfterAgent|TurnComplete) phase=waiting; detail='ready for direction' ;;
    SessionStart) phase=idle; detail='session ready' ;;
    SessionEnd) rm -f "$path" "$wp"; return ;;
    *) return ;;
  esac
  host=$(host_pid)
  [[ $host =~ ^[0-9]+$ ]] || return
  write_record "$path" "$phase" "$detail" "$id" "$session" || return
  pid=$(cat "$wp" 2>/dev/null)
  if [[ $pid =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]]; then
    cmd=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)
    if [[ $cmd == *"$SCRIPT --watch $path $host $id"* ]]; then return; fi
  fi
  # The child closes this lock fd and opens its own after setsid.
  setsid "$BASH" "$SCRIPT" --watch "$path" "$host" "$id" {lockfd}>&- </dev/null >/dev/null 2>&1 &
  printf '%s\n' "$!" > "$wp"
  chmod 600 "$wp"
}
main "$@"
exit 0
