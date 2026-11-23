#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail
src=$(cd "$(dirname "$0")" && pwd)
interpreter=$(command -v bash)
GOJQ=$(command -v gojq)
dest=$HOME/.local/share/agent-bridge-hooks
claude_settings=$HOME/.claude/settings.json
codex_hooks=$HOME/.codex/hooks.json
mode=${1:-install}
[[ $mode == install || $mode == --uninstall ]] || { echo 'usage: setup.sh [--uninstall]' >&2; exit 2; }

update_file() {
  local path=$1 provider=$2 command=$3 tmp backup
  mkdir -p -m 700 "$(dirname "$path")"
  tmp=$(mktemp "${path}.tmp.XXXXXXXX")
  chmod 600 "$tmp"
  if [[ -f $path ]]; then
    backup=$path.backup.$(date -u +%Y%m%dT%H%M%SZ)
    cp -p "$path" "$backup"
  else
    printf '{}\n' > "$path"
  fi
  if [[ $mode == --uninstall ]]; then
    "$GOJQ" --arg command "$command" '
      .hooks = ((.hooks // {}) | with_entries(.value |= (map(.hooks |= (map(select(.command != $command)))) | map(select(.hooks | length > 0)))) | with_entries(select(.value | length > 0)))
      | if .hooks == {} then del(.hooks) else . end' "$path" > "$tmp"
  else
    "$GOJQ" --arg command "$command" '
      .hooks = (.hooks // {}) |
      reduce ["SessionStart","UserPromptSubmit","PreToolUse","PostToolUse","PermissionRequest","Stop","SessionEnd"][] as $event
        (.; .hooks[$event] = ((.hooks[$event] // []) | map(.hooks |= map(select(.command != $command))) | map(select(.hooks | length > 0)) + [{hooks:[{type:"command",command:$command,timeout:3}]}]))' "$path" > "$tmp"
  fi
  mv -f "$tmp" "$path"
}

claude_command="$interpreter $dest/claude-activity-hook.py"
codex_command="$interpreter $dest/codex-activity-hook.py"
if [[ $mode == install ]]; then
  mkdir -p -m 700 "$dest"
  install -m 600 "$src/activity-lib.sh" "$dest/activity-lib.sh"
  install -m 700 "$src/claude-activity-hook.py" "$dest/claude-activity-hook.py"
  install -m 700 "$src/codex-activity-hook.py" "$dest/codex-activity-hook.py"
fi
update_file "$claude_settings" claude "$claude_command"
update_file "$codex_hooks" codex "$codex_command"
if [[ $mode == --uninstall ]]; then rm -f "$dest/activity-lib.sh" "$dest/claude-activity-hook.py" "$dest/codex-activity-hook.py"; rmdir "$dest" 2>/dev/null || true; fi
