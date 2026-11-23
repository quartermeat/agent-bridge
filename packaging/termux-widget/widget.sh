#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
widget_home=${WIDGET_HOME:-$HOME}
export PATH="$HOME/.local/bin:$PATH"
shortcuts=$widget_home/.shortcuts
state_dir=$widget_home/.local/state/agent-bridge/widget
marker='# agent-bridge-termux-widget:v1'
snapshot_file=
manual_refresh=0

usage() { printf 'usage: %s refresh [--manual] [--snapshot FILE] | open KIND ID [PID START] | watch\n' "$0" >&2; exit 2; }

label() {
  local value=${1##*/}
  value=${value%.sh}
  value=$(printf '%s' "$value" | tr '\n' '_' | LC_ALL=C sed -E 's/[[:cntrl:]]/_/g; s/[^A-Za-z0-9._ -]/_/g; s/ +/ /g; s/^[ ._-]+//; s/[ ._-]+$//' | cut -c 1-40)
  printf '%s' "${value:-project}"
}

owned() { [[ -f $1 && ! -L $1 && $(head -n 2 -- "$1" | tail -n 1) == "$marker" ]]; }

choose_decoder() {
  if [[ -x $HOME/.local/bin/gojq ]]; then printf '%s\n' "$HOME/.local/bin/gojq"
  elif command -v gojq >/dev/null 2>&1; then command -v gojq
  elif command -v jq >/dev/null 2>&1; then command -v jq
  else printf 'widget: install gojq or jq\n' >&2; return 1
  fi
}

decode() { "$decoder" -s -j -f "$script_dir/snapshot.jq" "$1" > "$rows"; }

# Background tasks avoid creating terminals when a row is tapped.
add_task() {
  local title=$1 kind=$2 id=$3 pid=${4:-} start=${5:-} qkind qid qpid qstart
  printf -v qkind '%q' "$kind"; printf -v qid '%q' "$id"
  printf -v qpid '%q' "$pid"; printf -v qstart '%q' "$start"
  {
    printf '#!/data/data/com.termux/files/usr/bin/bash\n%s\n' "$marker"
    printf 'exec %q open %s %s %s %s\n' "$script_dir/widget.sh" "$qkind" "$qid" "$qpid" "$qstart"
  } > "$desired/$title"
  chmod 700 "$desired/$title"
}

# Linux stat field 22 identifies a PID lifetime; field 7 identifies its controlling tty.
proc_fields() {
  local stat
  [[ $1 =~ ^[1-9][0-9]*$ ]] || return 1
  IFS= read -r stat < "${WIDGET_PROC_ROOT:-/proc}/$1/stat" || return 1
  [[ $stat == *') '* ]] || return 1
  read -ra proc_parts <<< "${stat##*) }"
  [[ ${#proc_parts[@]} -gt 19 ]] || return 1
  proc_start=${proc_parts[19]}
  proc_tty=${proc_parts[4]}
  proc_parent=${proc_parts[1]}
  [[ $proc_start =~ ^[0-9]+$ && $proc_parent =~ ^[0-9]+$ ]]
}

# Only a live Termux-owned process with a real controlling PTY can cause an app launch.
native_terminal() {
  local pid=$1 start=$2 root=${WIDGET_PROC_ROOT:-/proc} app_root=${WIDGET_TERMUX_ROOT:-/data/data/com.termux/files}
  local exe fd uid current parent first depth
  proc_fields "$pid" || return 1
  [[ $proc_start == "$start" && $proc_tty =~ ^[1-9][0-9]*$ ]] || return 1
  uid=$(awk '/^Uid:/ {print $2}' "$root/$pid/status") || return 1
  [[ $uid == "$(id -u)" ]] || return 1
  exe=$(readlink "$root/$pid/exe") || return 1
  [[ $exe == "$app_root/"* ]] || return 1
  fd=$(readlink "$root/$pid/fd/0") || return 1
  [[ $fd =~ ^/dev/pts/[0-9]+$ ]] || return 1
  current=$proc_parent
  for (( depth=0; depth<32 && current>1; depth++ )); do
    first=$(tr '\0' '\n' < "$root/$current/cmdline" | head -n 1) || return 1
    [[ $first == com.termux ]] && return 0
    proc_fields "$current" || return 1
    parent=$proc_parent
    [[ $parent != "$current" ]] || return 1
    current=$parent
  done
  return 1
}

# Every tap checks a new snapshot. The job helper verifies authoritative job metadata too.
open_row() {
  local kind=$1 id=$2 expected_pid=${3:-} expected_start=${4:-} raw rows decoder
  local row_kind row_id provider name cwd row_state launcher pid target_kind target_name health=unavailable found=0
  [[ $kind == status ]] && return 0
  [[ $kind == job || $kind == hook ]] || usage
  mkdir -p -- "$state_dir"
  raw=$(mktemp "$state_dir/open.XXXXXXXX")
  rows=$(mktemp "$state_dir/openrows.XXXXXXXX")
  trap 'rm -f -- "$raw" "$rows"' RETURN
  decoder=$(choose_decoder)
  if [[ -n ${WIDGET_LIVE_SNAPSHOT:-} ]]; then
    cp -- "$WIDGET_LIVE_SNAPSHOT" "$raw"
  else
    agent-bridge snapshot > "$raw" || return 0
  fi
  decode "$raw" || return 0
  while IFS= read -r -d '' row_kind && IFS= read -r -d '' row_id && IFS= read -r -d '' provider && IFS= read -r -d '' name && IFS= read -r -d '' cwd && IFS= read -r -d '' row_state && IFS= read -r -d '' launcher && IFS= read -r -d '' pid && IFS= read -r -d '' target_kind && IFS= read -r -d '' target_name; do
    if [[ $row_kind == health ]]; then health=$row_state; continue; fi
    if [[ $row_kind == "$kind" && $row_id == "$id" ]]; then
      (( found == 0 )) || return 0
      found=1
      selected_launcher=$launcher selected_pid=$pid selected_target_kind=$target_kind selected_target_name=$target_name
    fi
  done < "$rows"
  [[ $health == available && $found == 1 ]] || return 0
  if [[ $kind == job ]]; then
    [[ $selected_launcher == direct ]] || return 0
    "${WIDGET_JOB_VIEW_BIN:-widget-job-view}" "$id"
  else
    [[ -n $expected_pid && -n $expected_start && $selected_pid == "$expected_pid" ]] || return 0
    # Stock Termux has no safe API to select a particular native tab. An explicit
    # target remains available in the snapshot, but is never guessed from a label.
    native_terminal "$selected_pid" "$expected_start" || return 0
    "${WIDGET_ACTIVITY_CMD:-am}" start -a android.intent.action.MAIN -c android.intent.category.LAUNCHER -n com.termux/.app.TermuxActivity >/dev/null
  fi
}
refresh() {
  local raw rows desired decoder health=unavailable changed=0 existing file tmp kind id provider name cwd row_state launcher pid target_kind target_name base title digest start
  local -a ids=() kinds=() bases=() displays=() pids=() starts=()
  local -A counts=() used=()
  mkdir -p -- "$shortcuts/tasks" "$state_dir"
  chmod 700 "$state_dir"
  # Purge provisional HTML copies left by older widget versions.
  if [[ -d $state_dir/views && ! -L $state_dir/views ]]; then
    find "$state_dir/views" -maxdepth 1 -type f -name "*.html" -delete
    rmdir "$state_dir/views" 2>/dev/null || true
  fi
  exec 8>"$state_dir/refresh.lock"
  flock 8
  raw=$(mktemp "$state_dir/snapshot.XXXXXXXX")
  rows=$(mktemp "$state_dir/rows.XXXXXXXX")
  desired=$(mktemp -d "$state_dir/desired.XXXXXXXX")
  trap 'rm -f -- "$raw" "$rows"; rm -rf -- "$desired"' RETURN
  decoder=$(choose_decoder)
  if [[ -n $snapshot_file ]]; then
    cp -- "$snapshot_file" "$raw"
  elif ! agent-bridge latest > "$raw" || ! decode "$raw"; then
    agent-bridge snapshot > "$raw" || : > "$raw"
  fi
  if [[ -s $raw ]] && decode "$raw"; then
    while IFS= read -r -d '' kind && IFS= read -r -d '' id && IFS= read -r -d '' provider && IFS= read -r -d '' name && IFS= read -r -d '' cwd && IFS= read -r -d '' row_state && IFS= read -r -d '' launcher && IFS= read -r -d '' pid && IFS= read -r -d '' target_kind && IFS= read -r -d '' target_name; do
      if [[ $kind == health ]]; then
        health=$row_state
      elif [[ $kind == job || $kind == hook ]]; then
        if [[ -z $id || $cwd != /* || ! -d $cwd ]]; then health=unavailable; continue; fi
        [[ $kind != job || $launcher == direct ]] || continue
        base=$(label "$name")
        display=$(label "$provider")
        case ${provider,,} in codex) display=Codex;; claude) display=Claude;; *) display=${display^};; esac
        start=
        if [[ $kind == hook ]] && proc_fields "$pid"; then start=$proc_start; fi
        ids+=("$id"); kinds+=("$kind"); bases+=("$base"); displays+=("$display"); pids+=("$pid"); starts+=("$start")
        counts["$display · $base"]=$(( ${counts["$display · $base"]:-0} + 1 ))
      fi
    done < "$rows"
  fi
  if [[ $health == available ]]; then
    for (( i=0; i<${#ids[@]}; i++ )); do
      base=${bases[i]}
      title="${displays[i]} · $base"
      if (( ${counts["$title"]} > 1 )) || [[ -v used["$title"] ]] || { [[ -e $shortcuts/tasks/$title || -L $shortcuts/tasks/$title ]] && ! owned "$shortcuts/tasks/$title"; }; then
        digest=$(printf '%s\0%s' "${kinds[i]}" "${ids[i]}" | sha256sum); digest=${digest:0:6}
        title="${displays[i]} · $base · $digest"
      fi
      if [[ -v used["$title"] ]] || { [[ -e $shortcuts/tasks/$title || -L $shortcuts/tasks/$title ]] && ! owned "$shortcuts/tasks/$title"; }; then
        health=unavailable; break
      fi
      used["$title"]=1
      add_task "$title" "${kinds[i]}" "${ids[i]}" "${pids[i]}" "${starts[i]}"
    done
    if (( ${#ids[@]} == 0 )); then add_task 'Codex · No working agents' status ''; fi
  fi
  if [[ $health != available ]]; then
    rm -f -- "$desired"/*
    add_task 'Codex · Status unavailable' status ''
  fi
  {
    printf '#!/data/data/com.termux/files/usr/bin/bash\n%s\n' "$marker"
    printf 'exec %q refresh --manual\n' "$script_dir/widget.sh"
  } > "$desired/Refresh widget"
  chmod 700 "$desired/Refresh widget"
  shopt -s nullglob
  for file in "$desired"/*; do
    existing="$shortcuts/tasks/${file##*/}"
    if [[ -e $existing || -L $existing ]]; then
      if ! owned "$existing"; then
        printf 'widget: refusing to replace unowned task: %s\n' "$existing" >&2
        continue
      fi
      if cmp -s -- "$file" "$existing" && [[ -x $existing ]]; then continue; fi
    fi
    tmp=$(mktemp "$shortcuts/tasks/.ab.XXXXXXXX")
    cp -- "$file" "$tmp"
    chmod 700 "$tmp"
    mv -f -- "$tmp" "$existing"
    changed=1
  done
  for existing in "$shortcuts/tasks"/* "$shortcuts"/*; do
    owned "$existing" || continue
    [[ -e $desired/${existing##*/} && $existing == "$shortcuts/tasks/"* ]] && continue
    rm -f -- "$existing"
    changed=1
  done
  if (( changed || manual_refresh )) || [[ -e $state_dir/broadcast-pending ]]; then
    if "${WIDGET_REFRESH_CMD:-am}" broadcast -n com.termux.widget/.TermuxWidgetProvider -a com.termux.widget.ACTION_REFRESH_WIDGET --ei appWidgetId 0 >/dev/null; then
      rm -f -- "$state_dir/broadcast-pending"
    else
      : > "$state_dir/broadcast-pending"
      printf 'widget: refresh broadcast failed; retrying next refresh\n' >&2
    fi
  fi
}

case ${1:-} in
  refresh)
    shift
    while (( $# )); do
      case $1 in
        --manual) manual_refresh=1; shift ;;
        --snapshot) (( $# == 2 )) || usage; snapshot_file=$2; shift 2 ;;
        *) usage ;;
      esac
    done
    refresh
    ;;
  open)
    (( $# == 3 || $# == 5 )) || usage
    open_row "$2" "$3" "${4:-}" "${5:-}"
    ;;
  watch)
    (( $# == 1 )) || usage
    mkdir -p -- "$state_dir"
    exec 9>"$state_dir/watch.lock"
    flock -n 9 || exit 0
    while :; do
      "$script_dir/widget.sh" refresh || true
      sleep "${WIDGET_INTERVAL:-5}"
    done
    ;;
  *) usage ;;
esac
