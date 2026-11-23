#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
test_root=$(mktemp -d "${TMPDIR:-/data/data/com.termux/files/usr/tmp}/ab-widget-test.XXXXXXXX")
trap 'rm -rf -- "$test_root"' EXIT
home=$test_root/home
project="$test_root/project 'quote' \$(touch PWN)"
other=$test_root/other
mkdir -p -- "$home/.shortcuts/tasks" "$project" "$other" "$test_root/proc/999" "$test_root/native"
printf 'user shortcut\n' > "$home/.shortcuts/Codex · custom"
printf 'user shortcut\n' > "$home/.shortcuts/tasks/Claude · custom"
printf '#!/bin/sh\n# agent-bridge-termux-widget:v1\nexec codex\n' > "$home/.shortcuts/Codex · old row"
printf '#!/bin/sh\n# agent-bridge-termux-widget:v1\nexec codex\n' > "$home/.shortcuts/Codex · shared"
printf 'com.termux\0' > "$test_root/proc/999/cmdline"
proc_fixture() {
  local pid=$1 start=$2 tty=${3:-9} parent=${4:-999} n
  mkdir -p "$test_root/proc/$pid/fd"
  { printf '%s (agent) S %s 1 1 %s' "$pid" "$parent" "$tty"; for ((n=0;n<14;n++)); do printf ' 0'; done; printf ' %s 0\n' "$start"; } > "$test_root/proc/$pid/stat"
  printf 'Uid:\t%s\t%s\t%s\t%s\n' "$(id -u)" "$(id -u)" "$(id -u)" "$(id -u)" > "$test_root/proc/$pid/status"
  ln -sfn "$test_root/native/agent" "$test_root/proc/$pid/exe"
  ln -sfn /dev/pts/3 "$test_root/proc/$pid/fd/0"
}
proc_fixture 111 101
proc_fixture 222 202
proc_fixture 1234 303
cat > "$test_root/refresh-stub" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
printf 'refresh\n' >> "$WIDGET_TEST_REFRESH_LOG"
[[ ! -e $WIDGET_TEST_FAIL ]]
STUB
cat > "$test_root/view-stub" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
printf '%s\n' "$*" >> "$WIDGET_TEST_VIEW_LOG"
STUB
cat > "$test_root/am-stub" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
printf '%s\n' "$*" >> "$WIDGET_TEST_AM_LOG"
STUB
chmod 700 "$test_root/refresh-stub" "$test_root/view-stub" "$test_root/am-stub"
export WIDGET_HOME=$home WIDGET_PROC_ROOT=$test_root/proc WIDGET_TERMUX_ROOT=$test_root
export WIDGET_REFRESH_CMD=$test_root/refresh-stub WIDGET_TEST_REFRESH_LOG=$test_root/refresh.log
export WIDGET_TEST_FAIL=$test_root/fail WIDGET_TEST_VIEW_LOG=$test_root/view.log
export WIDGET_TEST_AM_LOG=$test_root/am.log WIDGET_ACTIVITY_CMD=$test_root/am-stub
export WIDGET_JOB_VIEW_BIN=$test_root/view-stub WIDGET_LIVE_SNAPSHOT=$test_root/live.json
export PATH="$HOME/.local/bin:$PATH"
decoder=$(command -v gojq || command -v jq)
"$decoder" -n -c --arg p "$project" --arg o "$other" '{version:1,sources:[{name:"bridge-jobs",available:true},{name:"activity-hooks",available:true}],jobs:[{id:"one",agent:"codex",name:"shared",cwd:$p,status:"running",launcher:"direct"},{id:"two",agent:"codex",name:"shared",cwd:$p,status:"running",launcher:"direct"},{id:"three",agent:"claude",name:"shared",cwd:$o,status:"running",launcher:"direct"},{id:"queued",agent:"codex",name:"queued",cwd:$p,status:"queued"},{id:"shell",agent:"codex",name:"shell",cwd:$p,status:"running",launcher:"tmux"}],agents:[{id:"hook-one",provider:"claude",name:"interactive",cwd:$p,activity_state:"active",host_pid:1234,target:{kind:"tmux",name:"actual"}},{id:"hook-idle",provider:"codex",name:"idle",cwd:$p,activity_state:"idle"}]}' > "$test_root/active.json"
"$decoder" -n -c '{version:1,sources:[{name:"bridge-jobs",available:true}],jobs:[]}' > "$test_root/idle.json"
"$decoder" -n -c '{version:1,sources:[{name:"bridge-jobs",available:false}],jobs:[]}' > "$test_root/down.json"
cp "$test_root/active.json" "$WIDGET_LIVE_SNAPSHOT"
count_refreshes() { wc -l < "$test_root/refresh.log"; }
count_views() { if [[ -f $WIDGET_TEST_VIEW_LOG ]]; then wc -l < "$WIDGET_TEST_VIEW_LOG"; else printf '0\n'; fi; }
count_activities() { if [[ -f $WIDGET_TEST_AM_LOG ]]; then wc -l < "$WIDGET_TEST_AM_LOG"; else printf '0\n'; fi; }
mkdir -p "$home/.local/state/agent-bridge/widget/views"
printf "old event copy\n" > "$home/.local/state/agent-bridge/widget/views/old.html"
"$source_dir/widget.sh" refresh --snapshot "$test_root/active.json"
[[ $(count_refreshes) -eq 1 ]]
[[ ! -e "$home/.shortcuts/Codex · old row" && ! -e "$home/.shortcuts/Codex · shared" ]]
[[ -f "$home/.shortcuts/Codex · custom" && -f "$home/.shortcuts/tasks/Claude · custom" ]]
mapfile -t duplicates < <(find "$home/.shortcuts/tasks" -maxdepth 1 -type f -name 'Codex · shared · *' | sort)
[[ ${#duplicates[@]} -eq 2 ]]
[[ -x "$home/.shortcuts/tasks/Claude · shared" && -x "$home/.shortcuts/tasks/Claude · interactive" ]]
[[ ! -e "$home/.shortcuts/tasks/Codex · queued" && ! -e "$home/.shortcuts/tasks/Codex · shell" && ! -e "$home/.shortcuts/tasks/Codex · idle" ]]
for file in "${duplicates[@]}"; do bash -n "$file"; "$file"; done
[[ $(count_views) -eq 2 ]]
sort "$WIDGET_TEST_VIEW_LOG" > "$test_root/sorted.log"
printf 'one\ntwo\n' > "$test_root/expected.log"
cmp -s "$test_root/expected.log" "$test_root/sorted.log"
"${duplicates[0]}"
[[ $(count_views) -eq 3 ]]
"$home/.shortcuts/tasks/Claude · shared"
[[ $(count_views) -eq 4 ]]
"$home/.shortcuts/tasks/Claude · interactive"
[[ $(count_activities) -eq 1 ]]
grep -Fq 'start -a android.intent.action.MAIN -c android.intent.category.LAUNCHER -n com.termux/.app.TermuxActivity' "$WIDGET_TEST_AM_LOG"
[[ ! -e $project/PWN && ! -d $home/.local/state/agent-bridge/widget/views ]]
# Stale shortcuts are inert before the next refresh.
cp "$test_root/idle.json" "$WIDGET_LIVE_SNAPSHOT"
"${duplicates[0]}"; "$home/.shortcuts/tasks/Claude · interactive"
[[ $(count_views) -eq 4 && $(count_activities) -eq 1 ]]
"$source_dir/widget.sh" refresh --snapshot "$test_root/idle.json"
[[ -x "$home/.shortcuts/tasks/Codex · No working agents" && $(count_refreshes) -eq 2 ]]
[[ ! -e ${duplicates[0]} && ! -e ${duplicates[1]} ]]
"$home/.shortcuts/tasks/Codex · No working agents"
"$source_dir/widget.sh" refresh --snapshot "$test_root/idle.json"
[[ $(count_refreshes) -eq 2 && $(count_views) -eq 4 ]]
: > "$test_root/fail"
"$source_dir/widget.sh" refresh --manual --snapshot "$test_root/idle.json" 2> "$test_root/error.log"
[[ -e $home/.local/state/agent-bridge/widget/broadcast-pending ]]
rm "$test_root/fail"
"$source_dir/widget.sh" refresh --snapshot "$test_root/down.json"
[[ -x "$home/.shortcuts/tasks/Codex · Status unavailable" && ! -e $home/.local/state/agent-bridge/widget/broadcast-pending ]]
"$home/.shortcuts/tasks/Codex · Status unavailable"
printf '{bad json\n' > "$test_root/bad.json"
"$source_dir/widget.sh" refresh --snapshot "$test_root/bad.json" 2> "$test_root/error.log"
[[ -x "$home/.shortcuts/tasks/Codex · Status unavailable" ]]
"$decoder" -n -c --arg p "$project" '{version:1,sources:[{name:"bridge-jobs",available:true}],jobs:[{id:"bad",agent:"claude",name:"bad\u0000name",cwd:$p,status:"running"}]}' > "$test_root/nul.json"
"$source_dir/widget.sh" refresh --snapshot "$test_root/nul.json" 2> "$test_root/error.log"
[[ -x "$home/.shortcuts/tasks/Codex · Status unavailable" ]]
task="$home/.shortcuts/tasks/Refresh widget"
printf 'user task\n' > "$task"
"$source_dir/widget.sh" refresh --snapshot "$test_root/idle.json" 2> "$test_root/error.log"
[[ $(cat "$task") == 'user task' ]]
grep -Fq 'refusing to replace unowned task' "$test_root/error.log"
# Isolated setup installs supplied helper and is repeatable.
setup_home=$test_root/setup-home
mkdir -p -- "$setup_home/.local/bin" "$test_root/setup-bin"
printf 'existing profile content\n' > "$setup_home/.bash_profile"
cat > "$setup_home/.local/bin/gojq" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
exec "$WIDGET_TEST_DECODER" "$@"
STUB
cat > "$setup_home/.local/bin/agent-bridge" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
cat "$WIDGET_TEST_SNAPSHOT"
STUB
cat > "$test_root/setup-bin/nohup" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
printf 'watcher launch\n' >> "$WIDGET_TEST_TOOL_LOG"
STUB
chmod 700 "$setup_home/.local/bin/gojq" "$setup_home/.local/bin/agent-bridge" "$test_root/setup-bin/nohup"
(
  export HOME=$setup_home
  unset WIDGET_HOME
  export PATH="$test_root/setup-bin:/data/data/com.termux/files/usr/bin"
  export WIDGET_TEST_DECODER=$decoder WIDGET_TEST_SNAPSHOT=$test_root/idle.json WIDGET_TEST_TOOL_LOG=$test_root/tool.log
  mkdir -p -- "$HOME/.local/share/agent-bridge-widget"
  printf 'working widget\n' > "$HOME/.local/share/agent-bridge-widget/widget.sh"
  printf 'working decoder\n' > "$HOME/.local/share/agent-bridge-widget/snapshot.jq"
  if WIDGET_JOB_VIEW_BIN="$test_root/missing-viewer" "$source_dir/setup.sh" > "$test_root/setup-failed.log" 2>&1; then exit 1; fi
  [[ $(cat "$HOME/.local/share/agent-bridge-widget/widget.sh") == 'working widget' ]]
  [[ $(cat "$HOME/.local/share/agent-bridge-widget/snapshot.jq") == 'working decoder' ]]
  "$source_dir/setup.sh"
  "$source_dir/setup.sh"
  cmp -s "$WIDGET_JOB_VIEW_BIN" "$HOME/.local/bin/widget-job-view"
  [[ -x "$HOME/.shortcuts/tasks/Codex · No working agents" ]]
  [[ -x "$HOME/.shortcuts/tasks/Refresh widget" ]]
  before=$(count_refreshes)
  "$HOME/.shortcuts/tasks/Refresh widget"
  [[ $(count_refreshes) -eq $((before + 1)) ]]
  [[ $(grep -c '^existing profile content$' "$HOME/.bash_profile") -eq 1 ]]
  [[ $(grep -Fc '# agent-bridge-termux-widget:v1' "$HOME/.bash_profile") -eq 1 ]]
  for (( attempt=0; attempt<20; attempt++ )); do [[ -e $WIDGET_TEST_TOOL_LOG ]] && break; sleep 0.1; done
  [[ $(grep -c '^watcher launch$' "$WIDGET_TEST_TOOL_LOG") -eq 2 ]]
)
# Two same-label hooks stay distinct; PID lifetime and terminal attachment gate launcher intents.
"$decoder" -n -c --arg p "$project" '{version:1,sources:[{name:"bridge-jobs",available:true},{name:"activity-hooks",available:true}],jobs:[],agents:[{id:"native-one",provider:"claude",name:"same",cwd:$p,activity_state:"active",host_pid:111},{id:"native-two",provider:"claude",name:"same",cwd:$p,activity_state:"active",host_pid:222}]}' > "$test_root/hooks.json"
cp "$test_root/hooks.json" "$WIDGET_LIVE_SNAPSHOT"
"$source_dir/widget.sh" refresh --snapshot "$test_root/hooks.json" 2> "$test_root/error.log"
mapfile -t native_rows < <(find "$home/.shortcuts/tasks" -maxdepth 1 -type f -name 'Claude · same · *' | sort)
[[ ${#native_rows[@]} -eq 2 ]]
"${native_rows[0]}"; "${native_rows[1]}"; "${native_rows[0]}"
[[ $(count_activities) -eq 4 ]]
# A reused PID or detached process cannot open Termux.
proc_fixture 111 999
"${native_rows[0]}"; "${native_rows[1]}"
[[ $(count_activities) -eq 5 ]]
proc_fixture 222 202 0
before=$(count_activities)
"${native_rows[0]}"; "${native_rows[1]}"
[[ $(count_activities) -eq $before ]]
"$decoder" -n -c --arg p "$project" '{version:1,sources:[{name:"bridge-jobs",available:true},{name:"activity-hooks",available:true}],jobs:[],agents:[{id:"native-one",provider:"claude",name:"same",cwd:$p,activity_state:"active",host_pid:0}]}' > "$test_root/empty-target.json"
cp "$test_root/empty-target.json" "$WIDGET_LIVE_SNAPSHOT"
"${native_rows[0]}"; "${native_rows[1]}"
[[ $(count_activities) -eq $before ]]
# A tap reads the authoritative snapshot even when latest is missing or stale.
tap_home=$test_root/tap-home
mkdir -p "$tap_home/.local/bin"
cat > "$tap_home/.local/bin/agent-bridge" <<'STUB'
#!/data/data/com.termux/files/usr/bin/bash
printf '%s\n' "$1" >> "$WIDGET_TEST_BRIDGE_LOG"
case $1 in
  latest) [[ -f $WIDGET_TEST_CACHE ]] || exit 1; cat "$WIDGET_TEST_CACHE" ;;
  snapshot) cat "$WIDGET_TEST_AUTHORITATIVE" ;;
  *) exit 2 ;;
esac
STUB
chmod 700 "$tap_home/.local/bin/agent-bridge"
"$decoder" -n -c '{version:1,sources:[{name:"bridge-jobs",available:true}],jobs:[]}' > "$test_root/ended.json"
"$source_dir/widget.sh" refresh --snapshot "$test_root/active.json"
(
  export HOME=$tap_home WIDGET_LIVE_SNAPSHOT= WIDGET_TEST_CACHE=$test_root/cache.json
  export WIDGET_TEST_AUTHORITATIVE=$test_root/active.json WIDGET_TEST_BRIDGE_LOG=$test_root/bridge.log
  rm -f "$WIDGET_TEST_CACHE"
  before=$(count_views)
  "${duplicates[0]}"
  [[ $(count_views) -eq $((before + 1)) ]]
  cp "$test_root/active.json" "$WIDGET_TEST_CACHE"
  export WIDGET_TEST_AUTHORITATIVE=$test_root/ended.json
  "${duplicates[0]}"
  [[ $(count_views) -eq $((before + 1)) ]]
  printf 'snapshot\nsnapshot\n' > "$test_root/expected-bridge.log"
  cmp -s "$test_root/expected-bridge.log" "$WIDGET_TEST_BRIDGE_LOG"
)
printf 'widget fixtures: OK\n'
