# agent-bridge

One local interface for coordinating AI agents: Codex jobs, Grok and Ollama prompts, live Claude/Codex activity, token usage, and GPU VRAM. Full details: [docs/REFERENCE.md](docs/REFERENCE.md).

## Communication rule

Every message sent through agent-bridge is a YAML document of **50 words or fewer**, with no prose around it. Anything longer goes in a file, linked by `detail:`. The specs live in `~/AGENTS.md` ("Response length"); change them only there, and bump the version key.

Task message (coordinator → worker, the job's prompt):

```yaml
task: v1
goal: one sentence
owns: [path, ...]
avoid: [path, ...]
checks: [command, ...]
detail: /absolute/path/to/task.md
```

Report message (worker → coordinator, its final message):

```yaml
report: v2
status: done | partial | blocked | failed
summary: one sentence
checks: pass | fail | partial | none
detail: /absolute/path/to/report.md
```

The coordinator rejects messages that do not parse or exceed 50 words.

## Commands

```bash
agent-bridge start --cwd DIR --name NAME --prompt-file task.yaml [--write] [--spawned-by AGENT_ID] [--context PATH[,PATH...]] [--context-work]
agent-bridge ask [--provider grok|ollama] --model MODEL [--prompt-file F] [--name NAME]
agent-bridge ask --provider ollama --list-models
agent-bridge list | show JOB_ID | wait JOB_ID... | stop JOB_ID | ack JOB_ID
agent-bridge snapshot | latest | watch
```

- `start` launches a Codex job in a user systemd unit. Read-only by default; `--write` allows edits in that worktree only.
- `--context` passes selected files from the context root to a `start` job under the sharing policy; repeat it or separate paths with commas. Work-sensitive files require `--context-work`.
- `ask` sends one prompt to Grok (xAI API key required) or a local Ollama model and prints the reply. It cannot run tools.
- `wait` blocks until jobs finish. Coordinators wait on every job they start.
- `ack` clears a halted job's alert after it has been resolved.

## Monitoring

The `agent-bridge-monitor` user service samples every two seconds to `~/.local/state/agent-bridge/latest.json`:

- agents and jobs, with `activity_state` and token usage;
- halts (`usage_limit`, `rate_limit`, `context_limit`) with scope;
- GPU VRAM, top VRAM processes, and loaded Ollama models.

`watch` shows the same view live and turns red at 90% VRAM. The token feed is at `~/.local/state/agent-bridge/token-feed.tsv`.

## Build and check

```bash
gofmt -l . && go vet ./... && go test ./...
go build -o ~/.local/bin/agent-bridge .
systemctl --user restart agent-bridge-monitor.service
```

Every commit bumps `VERSION` and gets a matching annotated tag.
