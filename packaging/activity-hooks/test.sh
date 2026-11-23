#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail
src=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
bridge_bin=${AGENT_BRIDGE_BIN:-$HOME/.local/libexec/agent-bridge}
if [[ ! -x $bridge_bin ]]; then bridge_bin=$root/agent-bridge; (cd "$src/../.." && go build -o "$bridge_bin" .); fi
cleanup() { [[ -n ${host:-} ]] && kill "$host" 2>/dev/null || true; rm -rf "$root"; }
trap cleanup EXIT
export HOME=$root/home AGENT_BRIDGE_ACTIVITY_DIR=$root/activity AGENT_BRIDGE_GOJQ=$(command -v gojq)
mkdir -p "$HOME/.claude" "$HOME/.codex"
printf '{"theme":"auto","hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo existing"}]}]}}\n' > "$HOME/.claude/settings.json"
printf '{"other":true}\n' > "$HOME/.codex/hooks.json"
bash "$src/setup.sh"
bash "$src/setup.sh"
"$AGENT_BRIDGE_GOJQ" -e '.theme=="auto" and ([.hooks.Stop[].hooks[].command] | index("echo existing") != null) and ([.hooks.Stop[].hooks[].command | select(contains("claude-activity-hook.py"))] | length==1)' "$HOME/.claude/settings.json" >/dev/null
"$AGENT_BRIDGE_GOJQ" -e '.other==true and ([.hooks.Stop[].hooks[].command | select(contains("codex-activity-hook.py"))] | length==1)' "$HOME/.codex/hooks.json" >/dev/null
sleep 40 & host=$!
export AGENT_BRIDGE_TEST_HOST_PID=$host
for provider in claude codex; do
  printf '{"hook_event_name":"UserPromptSubmit","session_id":"session-%s","prompt":"PRIVATE SECRET"}\n' "$provider" | bash "$src/$provider-activity-hook.py"
done
sleep 2
for provider in claude codex; do
  count=$(find "$AGENT_BRIDGE_ACTIVITY_DIR" -name "$provider-*.json" | wc -l)
  [[ $count == 1 ]]
  path=$(find "$AGENT_BRIDGE_ACTIVITY_DIR" -name "$provider-*.json" | head -1)
  "$AGENT_BRIDGE_GOJQ" -e --arg s "session-$provider" '.session_id==$s and .phase=="thinking" and .report_origin=="event" and (.detail | contains("SECRET") | not)' "$path" >/dev/null
  [[ $(stat -c %a "$path") == 600 ]]
  [[ $(stat -c %a "$AGENT_BRIDGE_ACTIVITY_DIR") == 700 ]]
  pid=$(cat "${path%.json}.watch.pid")
  [[ -r /proc/$pid/cmdline ]]
  [[ $(tr '\0' '\n' < "/proc/$pid/cmdline" | wc -l) == 6 ]]
done
"$bridge_bin" snapshot | "$AGENT_BRIDGE_GOJQ" -e --argjson host "$host" '([.agents[] | select(.host_pid==$host and .activity_state=="active" and .session_id=="session-claude")] | length)==1 and ([.agents[] | select(.host_pid==$host and .activity_state=="active" and .session_id=="session-codex")] | length)==1' >/dev/null
sleep 12
for provider in claude codex; do
  path=$(find "$AGENT_BRIDGE_ACTIVITY_DIR" -name "$provider-*.json" | head -1)
  [[ -f $path ]]
  stamp=$("$AGENT_BRIDGE_GOJQ" -r .sampledAt "$path")
  age=$(( $(date +%s) - $(date -d "$stamp" +%s) ))
  [[ $age -le 5 ]]
done
"$bridge_bin" snapshot | "$AGENT_BRIDGE_GOJQ" -e --argjson host "$host" '([.agents[] | select(.host_pid==$host and .activity_state=="active")] | length)==2' >/dev/null
kill "$host"; wait "$host" 2>/dev/null || true; host=
sleep 3
[[ $(find "$AGENT_BRIDGE_ACTIVITY_DIR" -name '*.json' | wc -l) == 0 ]]
"$bridge_bin" snapshot | "$AGENT_BRIDGE_GOJQ" -e --argjson dead "$AGENT_BRIDGE_TEST_HOST_PID" '([.agents[] | select(.host_pid==$dead)] | length)==0' >/dev/null
bash "$src/setup.sh" --uninstall
"$AGENT_BRIDGE_GOJQ" -e '.theme=="auto" and ([.hooks.Stop[].hooks[].command] | index("echo existing") != null)' "$HOME/.claude/settings.json" >/dev/null
"$AGENT_BRIDGE_GOJQ" -e '.other==true and (.hooks? == null)' "$HOME/.codex/hooks.json" >/dev/null
printf 'activity hook tests passed\n'
AGENT_BRIDGE_BIN=$bridge_bin bash "$src/regression_test.sh"
