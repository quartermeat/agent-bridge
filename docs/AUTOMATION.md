# Replacing agent rituals with scripts and timers (EPM plan, 2026-10-27)

User, 2026-10-27: "Wherever we can, we want scripts and cron jobs to replace
things that the AI has to do manually from reading AGENTS.md and memories."

Rule of thumb: if a step in `~/AGENTS.md` or a memory says "always run X / read
Y / check Z", it becomes a command whose output is machine-readable, and where
it is time- or event-driven, a systemd user timer or a Claude/Codex hook runs it.
AGENTS.md then shrinks to "run `X`" instead of the procedure. Long-lived code is
Go (per AGENTS.md); timers are systemd user units, not cron.

## Inventory: manual step → replacement

| # | Manual step today (source) | Replacement | Trigger | Risk |
|---|---|---|---|---|
| A1 | Session playbook step 1: snapshot, list, worktree list, git status, read HANDOFF, report who is where (AGENTS.md) | `agent-bridge orient [--repo DIR]` prints one YAML: live agents + activity_state, open/halted jobs, worktrees with merged/dirty flags, unmerged branches, provider limits, timer health | SessionStart hook (Claude), Codex startup | none, read-only |
| A2 | Startup rundown of recent projects (AGENTS.md "Conversation startup") | `orient --projects` emits top-5 from repo-attention state | same hook | none |
| A3 | Read archive README + handoff-notes at start (AGENTS.md, memory know-the-user) | SessionStart hook injects those two files into context (private, never written anywhere) | same hook | low; stays local |
| A4 | Check Steinberger timer health (memory) | `OnFailure=` notify unit on repo-attention.service; `orient` reports last run | systemd | none |
| B1 | Ack halted jobs once a fallback finished (AGENTS.md halt rules) | launcher auto-acks a halted job when its `fallback_for` successor completes and its branch is merged to main | launcher loop | low |
| B2 | Deferred coder restarts after a usage reset (memory codex-usage-reset) | already v0.23.0 (`queued_usage_limit` + `retry_at`); retire the memory | — | done |
| B3 | Prune merged worktrees (playbook step 8) | `agent-bridge worktrees prune` removes clean worktrees whose branch is merged into main and no job owns; `--dry-run` available; daily timer applies it and logs removals | timer | medium: deletes; auto approved (Q1); never dirty, unmerged, or job-owned |
| C1 | Write task spec v1 + detail skeleton + paste report spec (AGENTS.md spec) | `agent-bridge task new NAME --owns … --checks …` writes yaml + detail.md with role line, report spec, do-not-commit | command | none |
| C2 | Parse/validate v2 report and word cap (AGENTS.md) | `agent-bridge report-check JOB` validates YAML, ≤50 words, detail file exists; `wait` runs it automatically and flags `report_invalid` | `wait` | none |
| C3 | Append issues.yaml entry by hand (AGENTS.md issue reporting) | `agent-bridge issue add --kind … --summary …` assigns next AB-NNN, validates 25-word cap | command | none |
| C4 | Layered integration via diff/apply (playbook step 5) | `agent-bridge integrate JOB… --into WORKTREE` applies each job's diff with `--3way`, stages between layers, runs checks | command | low |
| D1 | Version bump + `vX.Y.Z: ` prefix + annotated tag + trailer (AGENTS.md, memory) | `vrelease minor|patch -m "subject"` bumps VERSION, commits, tags; `commit-msg` + `pre-push` hooks reject missing prefix/tag/bump, installed by `vrelease install-hooks` | command + git hooks | low |
| D2 | "Run the full pipeline before every push" (memory) | `pre-push` hook runs `make ci` | git hook | none |
| E1 | 50-word reply cap (AGENTS.md) | Claude Stop hook counts words in the last reply; over 50 → hook feedback asks for a rewrite | Claude hook | low |
| E2 | Memories naming files/flags may be stale (memory rules) | weekly timer: `memory-lint` checks paths/commands named in memory files exist; writes a report, never edits memories | timer | none |

## Order (each is one Codex coder job; Go, in agent-bridge unless noted)

1. A1+A2+A4 `orient` (biggest payoff: every session's first ~10 tool calls become one).
2. C1+C2 `task new` / `report-check`.
3. D1+D2 `vrelease` (new small Go tool in `~/work/vrelease`, used by every repo).
4. B1 auto-ack, C3 `issue add`, C4 `integrate`.
5. B3 prune (automatic, per Q1), E2 memory-lint.
6. A3/E1 hooks: harness config in `~/.claude/settings.json` and Codex config.
   Approved by the user (Q2).

After each lands, the EPM edits AGENTS.md to replace the procedure with the
command, and retires the memories it makes redundant.

## Needs the user

- Q1 (answered 2026-10-27): "leftover worktrees that are done, should be deleted." B3 deletes automatically: clean, branch merged into main, and no live or queued job owns the worktree. Daily timer applies it; each removal is logged.
- Q2 (answered 2026-10-27): yes, approve both session hooks (A3 archive-notes injection at SessionStart, E1 50-word Stop check). Implemented by a Codex coder as harness config, after `orient`.
