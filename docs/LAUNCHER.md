# Launcher and EPM design

**L1 implemented (2026-10-26):** Claude backend, job roles and depth, request spool, deterministic launch rules, Ollama verdicts, worktree dispatch, escalation CLI, and named job profiles. Tool-call approval and local task execution remain L2/L3.

Status: approved direction (2026-10-26), milestone L1 under implementation.

## Problem

Sandboxed bridge jobs cannot run `agent-bridge start` (issue AB-001: the jobs
directory and `.git` refs are read-only inside the sandbox), so an engineering
project manager (EPM) job cannot spawn coding workers. Every launch has to be
relayed by the user-facing coordinator.

## Roles

| Role | Who | May request |
| --- | --- | --- |
| coordinator | any user-facing Codex or Claude session | an `epm` job |
| epm | a Claude bridge job (`--agent claude --role epm`) | tasks, routed local-model first |
| dispatcher / approver | local model (`qwen3-coder:30b` via Ollama) inside the launcher | spawns Claude (`coder` or `epm`) or Codex (`coder`) |
| coder (Claude) | Claude bridge job, `--role coder` | nothing |
| coder (Codex) | Codex bridge job, `--role coder` | nothing; Codex never manages |

Codex jobs are always `coder`. The launcher rejects any request whose requester
is a Codex job, and rejects `--agent codex --role epm`.

## Flow

1. A coordinator or EPM runs `agent-bridge request --file task.yaml`. This only
   writes a request file to the spool; it needs no access to the jobs directory.
2. `agent-bridge launcher` (user service, outside any sandbox) picks the request
   up, records the requester (job id or agent id), and applies the hard rules.
3. Rule-passing requests go to the local model. For `epm` requests from a
   coordinator it only approves or escalates. For tasks from an EPM it also
   picks the worker: `claude coder`, `codex coder`, or `claude epm` (sub-EPM),
   preferring the cheapest worker that fits. The EPM may state a preference;
   the model may override it only within the rules.
4. On approval the launcher creates `~/worktrees/<repo>-<task>` on branch
   `worker/<task>` from the repo's current `main`, then calls `start` with
   `--write` for that worktree, `--spawned-by` the requester, `--agent` and
   `--role`.
5. The requester polls `agent-bridge request-status ID` (reads the spool's
   result file) and gets the job id, then waits as usual.

## Request format (YAML, `launch/v1`)

```yaml
launch: v1
role: epm | coder
agent: claude | codex | auto      # auto = let the local model pick
profile: codex-std | claude-std | ... # optional ceiling; see LAUNCHER-PROFILES.md
repo: /home/quartermeat/work/<repo>
task: short-kebab-name
prompt_file: /absolute/path/to/task.yaml   # an Agent task spec v1 document
context: [path, ...]                       # optional, same rules as --context
reason: one sentence
```

## Hard rules (deterministic; the model cannot override)

- `repo` is a Git repository under `~/work`; the worktree goes under `~/worktrees`.
- The requester is a live, known agent or a running bridge job; Codex jobs and
  coder jobs cannot request.
- Nesting depth (coordinator = 0) is at most 3; live launcher-started jobs at
  most 4 (configurable). Over-cap requests wait in FIFO order.
- `context` passes the existing sharing policy (`never` refused, work-sensitive
  needs the existing flag, which the launcher never sets on its own).
- Requests are 0600 files owned by the user's UID; anything else is rejected.
- Rejections are final; the requester sees the rule that failed.

## Tool approvals inside Claude jobs

Claude jobs run `claude -p` with `--permission-prompt-tool`, reusing the
`voice-approval-mcp` pattern, but pointing at the launcher's approval socket.

1. Deterministic allow: Read/Grep/Glob; Edit/Write inside the job's own worktree;
   a short allowlist of build/test commands (`go test`, `go vet`, `make ci`,
   `gofmt`, `git status|diff|log|add`).
2. Deterministic deny: `git push|tag|merge|reset --hard`, `sudo`, writes outside
   the worktree, network fetch tools, reading `~/private` or credential paths.
3. Everything else goes to the local model, which answers `allow`, `deny` or
   `ask`. `ask` and any model error or timeout escalate to the user.

## Escalation to the user

Escalations land in `~/.local/state/agent-bridge/launcher/pending/` and raise a
desktop notification. The user resolves them with `agent-bridge approvals`,
`agent-bridge approve ID` and `agent-bridge deny ID`. Unanswered escalations
deny after a timeout (default 30 min for launches, 5 min for tool calls).

## Observability

Every decision (rule result, model prompt hash, model verdict and rationale,
final outcome) is appended to `~/.local/state/agent-bridge/launcher/decisions.jsonl`.
Jobs gain `agent`, `role` and `depth` fields shown by `list`, `show`, snapshot
and `watch`.

## Milestones

- **L1**: Claude backend and roles for `start`; spool, `request`, `launcher`
  with hard rules, local-model launch approval and dispatch, worktree creation,
  decision log, user escalation CLI, packaged user unit.
- **L2**: launcher approval socket for tool calls inside Claude jobs (local model
  approver), replacing per-call user prompts.
- **L3**: the local model executes small tasks itself before escalating to
  Claude or Codex.

## Open questions

- Should the local model do small tasks itself (L3), or only dispatch and approve?
- Is it acceptable that EPM-spawned work never reaches `main` without the
  coordinator's review? (Current rule: yes, integration stays with the
  coordinator.)
