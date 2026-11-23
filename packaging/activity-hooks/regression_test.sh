#!/data/data/com.termux/files/usr/bin/bash
# Regression tests for locking, watcher lifetime, the watchdog and host discovery.
set -euo pipefail
src=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
bridge_bin=${AGENT_BRIDGE_BIN:-$HOME/.local/libexec/agent-bridge}
hosts=()
cleanup() { for p in "${hosts[@]}"; do kill "$p" 2>/dev/null || true; done; sleep 2; rm -rf "$root"; }
trap cleanup EXIT
trap 'echo "FAILED line $LINENO" >&2' ERR
export HOME=$root/home AGENT_BRIDGE_ACTIVITY_DIR=$root/activity AGENT_BRIDGE_GOJQ=$(command -v gojq)
mkdir -p "$HOME"
J=$AGENT_BRIDGE_GOJQ
new_host() { sleep 120 & host=$!; hosts+=("$host"); export AGENT_BRIDGE_TEST_HOST_PID=$host; }
fire() { printf '{"hook_event_name":"%s","session_id":"%s"}\n' "$1" "$2" | bash "$src/claude-activity-hook.py"; }
base_of() { local h; h=$(printf 'claude:%s' "$1" | sha256sum); printf '%s/claude-%s' "$AGENT_BRIDGE_ACTIVITY_DIR" "${h:0:12}"; }
phase() { "$J" -r .phase "$1.json"; }
live_watcher() { [[ -r /proc/$(cat "$1.watch.pid")/cmdline ]]; }

# 1. The watcher and hooks serialize on one lock; Stop is never reverted.
new_host; fire UserPromptSubmit lock; b=$(base_of lock); sleep 1
exec 7> "$b.lock"; flock -x 7
before=$(sha256sum < "$b.json"); fire Stop lock & hook=$!
sleep 3
[[ $(sha256sum < "$b.json") == "$before" ]]
kill -0 "$hook"
flock -u 7; exec 7>&-; wait "$hook"
[[ $(phase "$b") == waiting ]]
for _ in 1 2 3 4 5 6 7 8; do fire UserPromptSubmit lock; fire Stop lock; done
sleep 3; [[ $(phase "$b") == waiting ]]
fire UserPromptSubmit lock; fire SessionEnd lock; sleep 3
[[ ! -e $b.json && ! -e $b.watch.pid ]]

# 2. A hook killed while holding the lock does not prevent a later watcher.
new_host; b=$(base_of killed)
bash -c 'exec 7> "$1"; flock -x 7; sleep 60 & wait' _ "$b.lock" & holder=$!
sleep 1; pkill -KILL -P "$holder"; kill -KILL "$holder"; wait "$holder" 2>/dev/null || true
fire UserPromptSubmit killed; sleep 1; live_watcher "$b"
w=$(cat "$b.watch.pid"); n=0
for fd in /proc/$w/fd/*; do [[ $(readlink "$fd") == "$b.lock" ]] && n=$((n+1)); done
[[ $n == 1 ]]

# 3. The watcher leads its own session and keeps the argv the monitor requires.
stat=$(< "/proc/$w/stat"); stat=${stat##*) }; read -ra f <<< "$stat"
[[ ${f[3]} == "$w" ]]
[[ $(tr '\0' '\n' < "/proc/$w/cmdline" | wc -l) == 6 ]]

# 4. A quiet active session becomes waiting, and a new event revives it.
new_host; b=$(base_of quiet)
AGENT_BRIDGE_ACTIVITY_QUIET_SECONDS=2 fire UserPromptSubmit quiet
[[ $(phase "$b") == thinking ]]
sleep 6
"$J" -e '.phase=="waiting" and .detail=="ready for direction" and .report_origin=="watchdog"' "$b.json" >/dev/null
fire UserPromptSubmit quiet
[[ $(phase "$b") == thinking ]]

# 5. host_pid finds the agent process, not the sh -c that runs the hook.
unset AGENT_BRIDGE_TEST_HOST_PID
mkdir -p "$root/share/claude-termux/lib" "$root/bin"
cp "$(command -v bash)" "$root/share/claude-termux/lib/ld-musl-aarch64.so.1"
cp "$(command -v bash)" "$root/bin/codex"
for provider in claude codex; do
  bin=$root/bin/codex; [[ $provider == codex ]] || bin=$root/share/claude-termux/lib/ld-musl-aarch64.so.1
  printf '{"hook_event_name":"UserPromptSubmit","session_id":"find-%s"}\n' "$provider" > "$root/in"
  "$bin" -c 'sh -c "true; bash $1 < $2; true"; sleep 60 & wait' _ "$src/$provider-activity-hook.py" "$root/in" & agent=$!
  hosts+=("$agent"); sleep 3
  "$bridge_bin" snapshot | "$J" -e --argjson pid "$agent" --arg s "find-$provider" \
    '[.agents[] | select(.host_pid==$pid and .session_id==$s and .activity_state=="active")] | length==1' >/dev/null
done
printf 'activity hook regression tests passed\n'
