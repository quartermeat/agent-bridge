# Spawned-job profiles

Companion to `LAUNCHER.md`. Every job the launcher starts runs under exactly one
named profile. The profile fixes the model and the job's bandwidth, meaning its
reasoning effort, wall-clock limit and concurrency slot. Requesters ask for a
profile by name. The local model may pick a *cheaper* profile, never a pricier
one. Only the user (`approve --profile`) can raise a job above the
requester's ceiling.

Status: implemented 2026-10-26 on the launcher branch.

## Profiles

| Profile | Agent | Model | Effort | Wall clock | Use for |
| --- | --- | --- | --- | --- | --- |
| `codex-quick` | codex | `gpt-6-sol` | low | 20 min | mechanical edits, renames, test fixes |
| `codex-std` | codex | `gpt-6-sol` | medium | 60 min | normal scoped implementation (default coder) |
| `codex-deep` | codex | `gpt-6-sol` | high | 120 min | hard bugs, subsystem work |
| `claude-quick` | claude | `claude-haiku-5-5` | low | 20 min | not usable (see below) |
| `claude-std` | claude | `claude-sonnet-5-5` | medium | 60 min | not usable (see below) |
| `claude-epm` | claude | `claude-sonnet-5-5` | high | 180 min | EPM: plan, split, request, review (default EPM) |
| `claude-deep` | claude | `claude-opus-5-5` | high | 120 min | EPM for design-heavy or cross-cutting planning; user-approved only |

Coders are Codex only. Since v0.20.1 `start` rejects `--agent claude --role coder`,
and an `epm` job accepts only `claude-epm` or `claude-deep`. That leaves
`claude-quick` and `claude-std` with no valid role, so no job can run under them.
They are still listed in `packaging/profiles.yaml` and in the cost order;
removing them is a code change for the coding worker and is not done here.
Claude profiles are for documents, requirements, schedules and EPM planning,
never for runnable code.

- Model and effort map to `--model` / `--effort` for `claude -p`, and to
  `-m` / `-c model_reasoning_effort=` for `codex exec`.
- Profiles live in `~/.config/agent-bridge/profiles.yaml` (versioned with the
  repo's `packaging/profiles.yaml` default), so models can change without code.
  An unknown profile name is a hard reject.
- Wall clock is enforced by the bridge: at the limit the job gets `stop`, and its
  status is `halted` with reason `time_limit`. Partial work is kept.

## Bandwidth rules (deterministic)

- **Concurrency:** at most 4 live launcher jobs in total, at most 2 Claude, and at most
  1 of `claude-deep`. A request over a cap waits in the spool (FIFO) and is not rejected.
- **Ceiling by requester:** a coordinator can grant any profile except
  `claude-deep`, but may only request an EPM, so in practice `claude-epm`.
  An EPM cannot request `claude-deep`. Without a requested profile the ceiling
  is the role's default: `codex-std` for a coder, `claude-epm` for an EPM. The
  model's choice may not rank above the ceiling. A sub-EPM is `claude-epm`, and
  its depth counts toward the depth cap.
- **Routing preference:** the local model picks the cheapest profile that fits.
  The order is `codex-quick` < `claude-quick` < `codex-std` < `claude-std` <
  `codex-deep` < `claude-epm` < `claude-deep`. Every coder is Codex
  (`codex-quick`, `codex-std`, `codex-deep`); Claude profiles are for EPMs only.
- **Usage limits:** if Codex shows `halted_usage` or a recent `usage_limit` halt,
  a coder request is queued and retried. It is never moved to a Claude profile,
  because Claude cannot be a coder. Codex-to-Codex downgrades are the only
  substitution a coder can get. The launcher never upgrades a profile to work
  around a limit.
- **Local model:** the dispatcher/approver runs `qwen3-coder:30b` with
  temperature 0, a 30 s timeout per verdict and one request at a time. A timeout
  escalates. Queued bounded tasks for this model, and the measurements that
  decide which classes it keeps, are proposed in `LOCAL-FEEDBACK.md`.

## Recording

`decisions.jsonl` and the job JSON record `profile`, `model`, `effort`,
`time_limit` and any substitution, so `list` and `watch` show what each job
was given.

## Local agents in the viewer

Local-model work must appear in `snapshot`, `latest` and `watch` alongside
Claude and Codex.

- **New provider `local`.** The snapshot accepts activity files whose id starts
  with `local-` (today only `claude-` and `codex-` pass). Ids are
  `local-<role>-<model>-<n>`, for example `local-dispatcher-qwen3-coder-30b-1`.
- **Who writes them.** The launcher's dispatcher/approver, and any bridge
  command that calls `ollamaCompletion` (`grok`, future L3 local workers),
  write an activity file in the same format and directory as the hooks. That
  file sets `phase` and `detail` (for example "judging launch request
  <id>" or "idle") and is refreshed while the agent works, then marked finished when it exits.
- **Activity state.** `activity_state` follows the existing rules: active while a
  verdict or completion is in flight, idle between requests. Jobs and
  escalations it created list it as `spawned_by`.
- **Tokens.** Each completion's `TokenUsage` (Ollama prompt/eval counts) goes
  to the agent row, and to `token-feed.tsv` under provider `local` with its own
  cursor, so the sound table's grains include local tokens. First confirm the
  Orbital Drift sound table's parser tolerates a new provider value; if it
  does not, gate local rows behind a feed version bump.
- **Passive models.** Models loaded by other Ollama clients (`/api/ps`) show
  as read-only `local-ollama-<model>` rows with `activity_state: loaded` and no
  token counts, so a narrator or other tool holding GPU memory is visible.
- **Watch.** `watch` shows local rows with a distinct `local` tag and the model
  name. The GPU panel already present stays as it is.
