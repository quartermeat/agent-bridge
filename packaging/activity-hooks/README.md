# Termux activity hooks

`setup.sh` copies Bash and gojq hook producers to `~/.local/share/agent-bridge-hooks`, then merges lifecycle hooks into `~/.claude/settings.json` and `~/.codex/hooks.json`. Existing settings and other hooks are preserved. Each modified settings file gets a timestamped backup. Run `bash setup.sh --uninstall` to remove these hooks and their installed scripts.

The producers send fixed status labels only. Prompt text, tool commands, responses, and tool output are never copied to activity records. Each session gets a private JSON record in `AGENT_BRIDGE_ACTIVITY_DIR` or `~/.local/state/digital-terrarium/agents`. A detached watcher refreshes active records and removes them when its agent process dies. Idle and ready records keep their last event timestamp. An active session with no hook event for 600 seconds (`AGENT_BRIDGE_ACTIVITY_QUIET_SECONDS`) is marked ready with a watchdog origin, because an interrupt fires no Stop event. A single tool call longer than that also goes quiet until its next event.

Codex CLI loads user hooks from `~/.codex/hooks.json` after they are reviewed. In a new Codex session, use `/hooks` to review and trust the new user hook definition. Project trust also affects project hooks, but these are user hooks. The installer does not start or change live agent sessions.

Run `bash test.sh` for an isolated HOME test. The `.py` suffix is required by agent-bridge monitor discovery; scripts execute with Bash because Python is absent on this Termux installation.
