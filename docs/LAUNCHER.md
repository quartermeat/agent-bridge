# Launcher and EPM design

**L1 implemented (2026-10-26):** Claude backend (EPM only), job roles and depth, request spool, deterministic launch rules, Ollama verdicts, worktree dispatch, escalation CLI, and named job profiles. Tool-call approval and local task execution remain L2/L3.

Status: approved direction (2026-10-26). This document describes v0.20.2 behavior unless a passage is marked **Proposal** (not built, not decided). Routing policy and gaps are in `ROUTING.md`; the measured local-model trial is in `LOCAL-FEEDBACK.md`.

## Operating rules

These come from the user's current instructions. Where code enforces them today, the section below says so.

- Codex is the only user-facing bot. Every Claude instance starts as an EPM.
- Claude handles documents, requirements and schedules. Claude never writes or runs project code.
- All runnable code work goes to a freshly spawned Codex coder job. A Codex master may code a task directly only when it takes under one minute; anything longer is delegated.
- EPM runnable code work uses freshly spawned Codex coder jobs, one per task.
- Recurring cron jobs are **Proposal**: the user has not set a recurrence policy (see `ROUTING.md`). Queued local-model decision tasks are a **Proposal** with a measured trial plan in `LOCAL-FEEDBACK.md`. The user wants frontier models to give the local model more bounded work and to monitor how it performs.

## Problem

Sandboxed bridge jobs cannot run `agent-bridge start` (issue AB-001: the jobs
directory and `.git` refs are read-only inside the sandbox), so an engineering
project manager (EPM) job cannot spawn coding workers. Every launch has to be
relayed by the user-facing coordinator.

## Roles

| Role | Who | May request |
| --- | --- | --- |
| coordinator (master) | the user-facing Codex session. The launcher accepts any live agent from the snapshot as a coordinator; that Codex is the only user-facing bot is policy, not a launcher check | an `epm` job only |
| epm | a Claude bridge job (`--agent claude --role epm`) | Codex coder jobs and sub-EPMs, routed local-model first |
| dispatcher / approver | local model (`qwen3-coder:30b` via Ollama) inside the launcher | approves, escalates or rejects; picks the profile; spawns a Claude `epm` or a Codex `coder` |
| coder | Codex bridge job, `--role coder` | nothing; Codex jobs never manage |

Codex jobs are always `coder` and Claude jobs are always `epm`. `validAgentRole`
rejects `--agent codex --role epm` and `--agent claude --role coder`, so a Claude
coder cannot be started by `start`, by a request, or by a model verdict. The
launcher also rejects any request whose requester is a Codex job or a coder job.

An EPM is told not to write or run project code, but v0.20.2 enforces this only
through its prompt. The launcher starts every job with `--write`, and a Claude
write job is given Edit, Write and a short allowlist of build and review
commands. Limiting an EPM to documentation paths is **Proposal** (`ROUTING.md`,
gap G3).

## Flow

1. A coordinator or EPM runs `agent-bridge request --file task.yaml`. This only
   writes a request file to the spool; it needs no access to the jobs directory.
2. `agent-bridge launcher` (user service, outside any sandbox) picks the request
   up, records the requester (job id or agent id), and applies the hard rules.
3. Rule-passing requests go to the local model. For `epm` requests from a
   coordinator it only approves or escalates. For tasks from an EPM it also
   picks the worker: `codex coder` or `claude epm` (sub-EPM), preferring the
   cheapest profile that fits. A coder is always Codex; `agent: auto` for a
   coder means Codex, and the verdict parser rejects any other choice. The EPM
   may state a preference; the model may override it only within the rules.
4. On approval the launcher creates `~/worktrees/<repo>-<task>` on branch
   `worker/<task>` from the repo's current `main`, then calls `start` with
   `--write` for that worktree, `--spawned-by` the requester, `--agent` and
   `--role`.
5. The requester polls `agent-bridge request-status ID` (reads the spool's
   result file) and gets the job id, then waits as usual.

## Request format (YAML, `launch/v1`)

```yaml
launch: v1
role: epm | coder                 # epm from a coordinator; coder only from an EPM
agent: claude | codex | auto      # epm = claude, coder = codex; auto lets the model pick within that
profile: claude-epm | codex-std | ... # optional ceiling; see LAUNCHER-PROFILES.md
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
- A coordinator may request only an `epm`; Codex may not hold the `epm` role and
  Claude may not hold the `coder` role.
- When Codex is out of usage, a coder request is queued for retry. It is never
  moved to Claude.
- Rejections are final; the requester sees the rule that failed.

## `start` defaults

**Existing (v0.20.2):** `agent-bridge start` defaults to `--agent codex`. With no
`--role`, Codex gets `coder` (profile `codex-std`) and `--agent claude` gets
`epm` (profile `claude-epm`), so every Claude job started by `start` is an EPM
without extra flags (`startRole`; `TestStartClaudeDefaultsToEPM`). An EPM needs
the `claude-epm` (default) or `claude-deep` profile. An explicit `--agent claude
--role coder` still fails with `Claude cannot run the coder role`
(`TestStartRejectsClaudeCoder`), and Codex still cannot hold `epm`. This covers
`start` and launcher requests only; whether the voice daemon and interactive
Claude sessions also start as EPMs is open (`ROUTING.md`, Q16).

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
  Claude or Codex. The first step is the measured trial in `LOCAL-FEEDBACK.md`
  (a serial queue with telemetry), which runs bounded tasks without tools.

## Open questions

- Should the local model do small tasks itself (L3), or only dispatch and approve?
- Which recurring work becomes a cron job? The user has not set a recurrence
  policy, so cron is not designed here. Queued local-model runs have a proposed
  design with a measured trial in `LOCAL-FEEDBACK.md`; it is not built.
- Is it acceptable that EPM-spawned work never reaches `main` without the
  coordinator's review? (Current rule: yes, integration stays with the
  coordinator.)
