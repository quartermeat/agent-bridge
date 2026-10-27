# Task routing: requirements and design draft

Status: draft for review (2026-10-26, revised after the user's role correction
the same day, and again for the user's local-model instruction). Nothing in this
document is implemented unless the "Existing behavior" section says so.
Companion to `LAUNCHER.md` (L1 launcher and EPM design), `LAUNCHER-PROFILES.md`
(job profiles) and `LOCAL-FEEDBACK.md` (measured local-model trial).

## Objective

The user's requests of 2026-10-26, restated as requirements:

1. Codex is the only agent the user talks to.
2. Every Claude instance is an EPM (engineering project manager) from its first
   turn. There is no Claude coder role.
3. The Codex master may write code itself **only when the task takes under one
   minute**. Anything longer is delegated. The user's words: "I want my
   instances of claude to be an epm right off the bat, I want you to be a coder
   first, only if it takes under a minute to perform. Anything other than that,
   I need you to delegate."
4. Codex creates an EPM in Claude or in a local model, according to task
   complexity.
5. EPMs never code. Their runnable code work goes only to freshly spawned Codex
   coders, under the organizational rules in `~/AGENTS.md`.
6. Documents, requirements and schedules go to Claude, as EPM work.
7. Simple tasks become a cron job. What counts as simple, and what counts as
   recurring, is **pending user clarification**.
8. Tasks whose decision tree is larger than cron go to the local model, which
   needs a queue.
9. Spawned Claude and Codex jobs run within their token or usage limits.
10. The user said this routing system should precede new GUP revenue work. When
    GUP work starts, and which routing milestone counts as "done enough", is the
    user's decision (Q10). This document sets no gate of its own.

11. Latest instruction, on the local model: "I need you frontier models to give
    more love to the local, monitor performance, so you have a feedback loop of
    what kind of tasks it can handle in an amount of time." This makes a
    **measured local trial** a priority: frontier Codex and Claude EPMs send the
    local model bounded work, record how it performs against a time budget, and
    use that record to decide which task classes it keeps. The design is in
    `LOCAL-FEEDBACK.md`. It does not change items 1 to 10, and it does not answer
    the open cron and GUP questions (Q1 to Q3, Q10).

Non-goals: autonomous publishing, financial transactions, GUP deployment, and
any change to GUP. This document does not touch GUP.

## Actors and boundaries

| Actor | Does | Must not |
| --- | --- | --- |
| User | Talks to the Codex master; resolves escalations; grants explicit approval for sensitive actions | n/a |
| Codex master (coordinator) | Classifies the task, writes the task spec, requests an EPM, reviews, integrates, commits and tags. May edit code itself **only** for a task that takes under one minute (route R-direct) | Edit code for anything that takes a minute or longer; code because a provider is limited; touch GUP |
| Claude EPM | The only role a Claude instance has, from its first turn. Plans and splits work, writes documents, requirements, schedules and task prompts, requests fresh Codex coders, reviews reports and diffs | Write or edit code; run project code; integrate to `main`; act as a coder for any duration |
| Local model | Runs queued decision-tree tasks and launch verdicts; may act as a local EPM once L3 exists | Write files or run commands on its own; write code; pick Claude or Codex coders outside the rules |
| Codex coder | Fresh job per task, own worktree and branch, owned paths, exact checks | Spawn jobs; resume an earlier session; widen its owned paths |
| Launcher (`agent-bridge launcher`) | Applies deterministic rules, asks the local model for verdicts, creates worktrees, starts jobs | Be overridden by any model verdict |
| Monitor | Publishes snapshots of agents, jobs, halts, GPU and tokens | Make routing decisions |

A documentation-only job is an EPM job whose `owns` list contains only document
paths. It is not a separate role.

Role boundary reminders from `~/AGENTS.md`, as amended by the user's correction
of 2026-10-26: the master delegates every non-document change except a task that
takes under one minute (the guide now says so: G11 is closed); limits do not
move coding authority to the master, so the exception never applies because a
provider is limited; integration (commit, tag, merge) stays with the master
after reviewing the report, diff and checks, including for work the master did
itself; no push, publish or deploy without explicit user intent.

The under-one-minute exception is an honor-system rule for the Codex master. The
bridge cannot see the master's own edits, so it cannot enforce the limit.
Proposed evidence: the master states the estimate and the elapsed time in its
status line. What the minute includes (checks, version bump, commit) is
Q15.

"Fresh" Codex means a new bridge job per task: new job ID, new worktree and
branch, no reused or resumed session, and no context beyond the task spec and
the files passed with `--context`.

"Code" means every path that is not a document. For enforcement, a document is a
file under `docs/` or a Markdown file or a task prompt. Everything else is code,
including tests, data, build files, configuration, assets and version files,
matching the master role boundary in `~/AGENTS.md`. A sub-minute change by the
Codex master to any of these paths is the only code written outside a coder job.

## Route table

The Codex master picks a route and states it in the request. The launcher
validates the route against hard rules; the local model may only pick a cheaper
option inside the route, never a different route class. A request cannot name a
Claude coder: the launcher rejects it (see "Strict Codex-only coding
enforcement").

| Route | Trigger (proposed rubric; pending confirmation) | Executor | Writes | Output |
| --- | --- | --- | --- | --- |
| R-direct: sub-minute change | A non-document change the master expects to take under one minute. Threshold and what it includes are pending Q15 | The Codex master itself, the only exception to delegation | The files the change needs, on a branch or worktree as for any other change | Diff, checks, and the usual reviewed commit |
| R-cron: one-off or simple recurring | Single deterministic command or script, no branching on its output, no judgment. "Simple" and "recurring" thresholds are pending user clarification | A scheduled job spec that runs without any model. The script itself is code, so a fresh Codex coder writes it (or R-direct if it is a sub-minute change) | Script and schedule entry, both code or configuration | Exit status and log |
| R-doc: documents, requirements, schedules | The deliverable is prose or a plan, no runnable code | Claude EPM that owns only documentation paths | Documentation paths only | Document plus report v2 |
| R-local: decision tree larger than cron | Bounded steps with branches decided by classification, triage or checks; no code authoring. A task class qualifies by the rule in `LOCAL-FEEDBACK.md`: it is on the allowed list, has a declared time budget, and is not on a frontier-only list. Until a class has measured evidence it runs as a trial (exploration or baseline pair), not as a trusted route | Local model through the local queue; a frontier agent reviews and keeps the safe fallback | Result files in the job state directory only | Structured result (JSON or YAML), log, and a telemetry record |
| R-code: coding | A non-document change that takes a minute or longer | Claude EPM or local EPM plans and splits; each unit of work runs in a fresh Codex coder | Owned paths in the worker's own worktree | Branch, report v2, check output |
| R-review: review and integration | A finished worker branch | Fresh read-only Codex job for review when checks must run; the EPM reviews reports and diffs; the Codex master integrates | Integration commit and tag by the master only | Reviewed diff, evidence, commit |

A Claude instance started without an explicit role is an EPM. `agent-bridge
start --agent claude` selects role `epm` and profile `claude-epm` by default
(landed in v0.20.2, gap G12 closed). An explicit `--agent claude --role coder`
still fails with "Claude cannot run the coder role".

Choosing the EPM host for R-code ("Claude or a local model according to task
complexity") needs a rubric. Proposed, not confirmed: use the local EPM when the
plan is a known playbook with enumerated branches and at most a few coders; use
a Claude EPM when the plan needs open-ended design, spans several subsystems,
or needs judgment about requirements. The rubric and its thresholds are an open
question (see the end of this document).

Out of every route: a task that implies publishing, a financial transaction or a
deployment is not routed. It is returned to the master as a request for explicit
user approval.

## Existing behavior and gaps

Implemented as of v0.20.2 (L1, the Codex-only coder restriction from v0.20.1,
and the Claude-defaults-to-EPM `start` default from v0.20.2, checked against
`launcher.go`, `profiles.go`, `main.go`, `localmodel.go` and the packaged
units). Locations are function names, because line numbers drift.

| Area | Behavior | Where |
| --- | --- | --- |
| Roles and depth | `coder` and `epm`; Codex cannot be `epm`; **Claude cannot be `coder`** (v0.20.1); depth cap 3 | `validAgentRole`, `checkLaunchRules` |
| Codex-only coder, request | `launch/v1` with `role: coder, agent: claude` is rejected at parse time and again in the hard rules; a profile whose agent cannot hold the role is rejected | `parseLaunchYAML`, `checkLaunchRules` |
| Codex-only coder, verdict | A model verdict may not change the requested agent or role; a Claude coder verdict is rejected; the model prompt says `agent: auto` for a coder means Codex | `parseModelVerdict`, `modelLaunchVerdict` |
| Codex-only coder, substitution | The Codex-to-Claude profile entries are gone; only Claude-to-Codex remains. When Codex is exhausted, a coder request is written as `queued_usage_limit` and retried, never moved to Claude | `substituteProfile`, `processLaunch` |
| Codex-only coder, user approval | A forced (user-approved) `agent: auto` coder resolves to Codex; `approve --profile` re-validates agent and role | `processLaunch`, `resolveApprovalCommand` |
| Codex-only coder, direct start | `agent-bridge start --agent claude --role coder` fails with "Claude cannot run the coder role" (`TestStartRejectsClaudeCoder`) | `start`, `validAgentRole` |
| Start defaults (v0.20.2) | `--agent claude` with no `--role` selects `epm` and profile `claude-epm`; Codex defaults to `coder` and `codex-std` (`TestStartClaudeDefaultsToEPM`) | `startRole`, `defaultProfile`, `start` |
| Requester rules | Coordinator may request only an `epm`; only a running Claude EPM job may request from a job; Codex and coder jobs cannot | `requesterDepth`, `checkLaunchRules` |
| Spool and CLI | `request`, `request-status`, `launcher`, `approvals`, `approve`, `deny`; private spool, FIFO by mtime | `requestCommand`, `launcherOnce` |
| Local verdict | Ollama model returns approve, escalate or reject as validated JSON; timeouts and errors escalate | `parseModelVerdict`, `modelLaunchVerdict` |
| Dispatch | Worktree at `~/worktrees/<repo>-<task>` on `worker/<task>`, then `start --write` with parent job and spawner | `dispatchLaunch` |
| Profiles | Seven named profiles, cost order, wall-clock limit enforced by the bridge | `profiles.go`, `packaging/profiles.yaml` |
| Concurrency | At most 4 live launcher jobs (`AGENT_BRIDGE_LAUNCHER_MAX_JOBS`), 2 Claude, 1 `claude-deep`; over-cap requests wait | `launcherSlotAvailable` |
| Usage backpressure | Reactive: a provider is exhausted if an agent is `halted_usage` or a job halted with `usage_limit` in the last hour; a coder waits, an EPM request escalates | `providerExhausted`, `processLaunch` |
| Escalation | Pending files, desktop notification, timeout deny | `notifyPending`, `expirePending` |
| Observability | `decisions.jsonl`, job fields `agent`, `role`, `depth`, `profile`, `substitution`; snapshot and `watch` | `appendDecision` |
| Local model | Pinned model service, lease and yield around GPU renders, completions via `ollamaCompletion` | `localmodel.go` |

Not yet true, although the master's instruction implies it: `claude-quick` and
`claude-std` remain as profiles that no role can use, and Claude write jobs,
EPM jobs included, still carry Edit and Write with no path restriction. The
launcher passes `--write` to every job it starts, so an EPM's inability to code
rests on its prompt alone. No tool in the bridge limits an EPM to documentation
paths today.

Planned, not implemented: **L2** (launcher approval socket for tool calls inside
Claude jobs) and **L3** (the local model executes small tasks itself). Neither
is relied on as existing behavior below; each gap that needs them says so.

Gaps against the objective:

| # | Gap | Evidence | Needed by |
| --- | --- | --- | --- |
| G1 | ~~Claude can be a coder.~~ **Closed in v0.20.1** for launcher requests, model verdicts, user approvals and direct `start --role coder`. Profiles `claude-quick` and `claude-std` still exist and are described as implementation profiles in `LAUNCHER-PROFILES.md` | `validAgentRole`; `LAUNCHER-PROFILES.md` | Q12 (retire, rename or keep for EPM children) |
| G2 | ~~Cross-provider substitution for coding.~~ **Closed in v0.20.1**: the Codex-to-Claude entries are removed, and a coder request waits as `queued_usage_limit` while Codex is limited. The wait has no visible retry time or expiry (G7, G9) | `substituteProfile`, `processLaunch` | M2, M3 |
| G3 | **EPMs can edit code.** The launcher always passes `--write`, and Claude write jobs get Edit and Write; the only barrier is the prompt. This is the largest remaining hole against "EPMs never code", and the under-one-minute exception does not extend to EPMs | `dispatchLaunch`, `README.md` (`start`) | M1c (path-limited tools if the Claude CLI supports them, otherwise L2) |
| G4 | **No local EPM.** A coordinator may launch only a Claude EPM, and the local model cannot run multi-step work | `parseModelVerdict`, `checkLaunchRules` | M5 |
| G5 | **No route concept.** Requests carry role and agent, not a route; there is no documentation-only `owns` check, no cron path and no local task queue | `launchRequest` | M4, M5 |
| G6 | **Head-of-line blocking.** One request waiting on capacity stops the pass (`break`), so a Codex request behind a capped Claude request also waits | `launcherOnce` | M2 |
| G7 | **Queue is invisible.** Spool and capacity waits are not in the snapshot, `list` or `watch`; there is no queue depth, age or deferral reason | `monitor.go`, `watch_render.go` have no spool reference | M2 |
| G8 | **No cancel, priority or expiry** for queued launch requests (pending approvals do expire) | `expirePending`, `launcherOnce` | M2 |
| G9 | **Backpressure is reactive and coarse.** No Codex cap, no deferral time, no per-job token budget; an exhausted provider is detected only after a job halts | `launcherSlotAvailable`, `providerExhausted` | M3 |
| G10 | **User still talks to more than Codex.** Escalations go to `approvals` and desktop notifications, and the voice daemon defaults to `--agent claude` with work access | `README.md` voice section | open question Q7, Q8 |
| G11 | ~~`~/AGENTS.md` still says the master never edits code and may choose Codex or Claude for each job.~~ **Closed**: the global guide's master and coding worker roles now say the Codex master may edit code only for a task under one minute, every Claude instance starts as an EPM, and coding workers are Codex only. Cron, voice, GUP timing and trial defaults are not part of this edit and stay open | `~/AGENTS.md` (master and coding worker roles) | Q1 to Q3, Q8, Q10 |
| G12 | ~~Claude does not start as an EPM by default.~~ **Closed in v0.20.2**: `start --agent claude` selects `epm` and `claude-epm`, and the help text states the defaults. Voice and interactive Claude sessions are not covered (Q16) | `startRole`, `start` | Q16 |
| G13 | **The sub-minute exception has no enforcement or evidence trail.** The bridge cannot see the master's own edits, so nothing checks the time estimate or that a Claude instance did not take the exception | n/a | Q15 |
| G14 | **Legacy job records.** `readJob` defaults an empty role to `coder` for any agent, so an old Claude job without a role displays as a Claude coder | `readJob` | Low priority; decide whether to default by agent |
| G15 | **No measured local use.** The local model only issues launch verdicts. Nothing sends it bounded work, records its wall time, tokens or result validity, or lets a frontier agent grade the output, so no one knows which tasks it can handle in what time. This is the gap the user's latest instruction targets | `launcher.go`, `localmodel.go`, `ollama.go` (completions are serialized in process but not queued or recorded as task outcomes) | M5a to M5c; design in `LOCAL-FEEDBACK.md` |

## Strict Codex-only coding enforcement

Requirement: no code reaches a worktree except through a fresh Codex coder job,
or a sub-minute edit by the Codex master (R-direct). Enforce in layers, because
a prompt alone does not enforce anything. Layers 1 to 3 are implemented
(v0.20.1); layers 4 to 8 are not.

1. **Request rule (deterministic). Implemented.** `role: coder` with
   `agent: claude` is rejected, and `agent: auto` for a coder resolves to Codex.
   The rejection shows in `request-status`; the rule string today is `Claude
   cannot run the coder role`. A stable `coding requires Codex` string is not
   required unless the user wants it.
2. **Verdict rule. Implemented.** The verdict parser rejects any model verdict
   that sets a coder to Claude, or that changes the requested agent or role,
   whatever the model returns.
3. **No substitution across providers for coding. Implemented.** The
   Codex-to-Claude entries are gone from the substitution table. When Codex is
   limited, the coder request waits as `queued_usage_limit`, never moved.
   Visible deferral reasons and retry times are still missing (M2, M3).
4. **Claude EPM jobs get document-only write access. Not implemented (G3).**
   Claude EPM jobs run with Edit and Write limited to the task's `owns` paths,
   and the launcher rejects any task whose `owns` includes a code path. The
   mechanism is path-limited tool permissions if the Claude CLI supports them;
   if it does not, the mechanism is the L2 approval socket, and M1c ships
   read-only EPM tools until L2 exists. This must be verified, not assumed
   (open question Q9).
5. **Bash for Claude jobs** stays on the existing short allowlist (build, test
   and read-only Git commands). No shell redirection or interpreter that could
   write code is on it.
6. **Local model writes nothing.** Its output is a result object the bridge
   validates and stores in the job state directory. It has no file or shell tool.
7. **After-the-fact audit.** When any Claude or local job ends, the bridge
   compares its worktree to the base commit. Any changed path outside the
   document allowlist marks the job `violation`, blocks integration and is
   logged in `decisions.jsonl`. The master must not integrate a `violation` job.
8. **Provenance.** Each worker branch records which job (agent, role, profile)
   produced it, so a reviewer can confirm every code commit came from a Codex
   coder job.

Failure behavior: an enforcement failure is a rejection or a deferral, never a
fallback to a different executor. A usage limit never authorizes the master or
Claude to code. The sub-minute exception is about task size, not provider
state: a limited Codex coder does not make a longer task eligible for R-direct.

## Queues and lifecycle

Three queues are needed. Only the first exists.

| Queue | Holds | Executor | Status |
| --- | --- | --- | --- |
| Launch queue | `launch/v1` requests for worker jobs | Launcher | Exists (spool) |
| Local queue | `local/v1` bounded tasks, durable, with telemetry (`LOCAL-FEEDBACK.md`) | Pinned local model, one at a time | Needed; first slice is M5a |
| Schedule registry | Cron or timer entries | Scheduler, no model | Needed (M4), gated on Q1 to Q3 |

Proposed unified record for any queued item:

```yaml
id: string
route: cron | doc | local | code | review
requester: {agent_id: string, job_id: string}   # as recorded today
priority: normal | high                         # user-raised only
created_at: timestamp
state: queued | deferred | pending_approval | dispatched | running | done | failed | halted | canceled | expired | rejected
deferred_until: timestamp                       # set only with a reason
defer_reason: capacity | provider_limit | gpu_yield | approval
attempts: int
ttl: duration
```

Lifecycle:

1. `submitted`: written to the spool by `request` (private, owned by the user).
2. Hard rules run. Failure is final: `rejected` with the rule string.
3. Verdict: the local model approves, escalates or rejects. Escalation becomes
   `pending_approval` and denies on timeout (existing).
4. Capacity and backpressure gates. Failure moves the item to `deferred` with a
   reason and a retry time; it is never dropped and never rerouted to another
   provider for coding.
5. `dispatched`: worktree created, job started; item links to the job ID.
6. Terminal states follow the job: `done`, `failed`, `halted` (usage, rate,
   context, time or budget limit) or `canceled`. A halted item keeps its
   partial work and a durable handoff, and is retried with an eligible worker
   or deferred until capacity returns.
7. `expired` when `ttl` passes while deferred; the requester sees the reason.

Ordering: FIFO within a provider and route, not globally, so a blocked provider
does not stop unrelated work (fixes G6). Priority raises an item only when the
user sets it.

Observability requirements:

- `agent-bridge queue [--json]` lists items with route, state, age, requester,
  defer reason and retry time.
- The snapshot gains a `queue` section: depth per route and provider, oldest
  age, and deferred counts with reasons. `watch` shows it, and `latest` carries
  it so Codex can read it quickly.
- Every state change appends to `decisions.jsonl` with the rule or verdict.
- Local-model work shows as `local` rows in the viewer with tokens, as already
  specified in `LAUNCHER-PROFILES.md`.
- A cancel command (`agent-bridge cancel REQUEST_ID`) removes a queued or
  deferred item and records who canceled it.

## Local queue and decision trees (R-local)

The local model has no tools today (`ask` cannot run tools), and L3 is
unimplemented. To run a decision tree safely, the **bridge executes the tree and
the model only chooses among enumerated branches**:

- A playbook is declarative YAML: ordered steps, each either a bridge-run
  command from an allowlist or a model classification with a fixed set of
  allowed labels, plus branch rules on labels or exit status.
- The model returns one label per classification step. Output outside the allowed
  set is an error, retried a bounded number of times, then escalated to the
  requester (and to a Claude EPM if the requester asks).
- One task runs at a time because one pinned model serves it. The queue pauses
  with reason `gpu_yield` while a lease is held by a GPU render, and resumes when
  the last lease ends.
- Each task has a timeout, a step limit and a token limit. Results are written
  under the job state directory.
- A playbook that needs to author or change code does not run locally. It emits
  an R-code request for a fresh Codex coder through the normal launcher.

The queue, its task classes, the time budget and telemetry record, the review
grades, and the way frontier agents choose trial tasks and adjust thresholds
are specified in `LOCAL-FEEDBACK.md`. The measured trial starts with single-step
tasks (one prompt, one validated result), which need no tools and no playbook
engine, and grows toward multi-step playbooks only for classes that have earned
it. Whether a local EPM is in scope at all, or the local model should remain a
dispatcher and playbook runner, is the existing open question in `LAUNCHER.md`
(L3) and is repeated below. The trial does not need that answer.

## Usage and concurrency backpressure

Requirement: spawned Claude and Codex jobs run within their token or usage
limits, without the master inferring an exhausted allowance from token totals
(`~/AGENTS.md`).

Existing: the caps and reactive detection listed above. Proposed additions:

1. **Per-provider caps.** A configurable Codex cap alongside the Claude cap,
   plus the total cap. Default values are a decision for the user, not part of
   this draft.
2. **Provider pause.** When halt evidence shows `usage_limit` or `rate_limit`
   for a provider, new requests for that provider become `deferred` with
   `defer_reason: provider_limit`. `deferred_until` comes from the reset time in
   the halt evidence when it can be parsed, otherwise from a bounded backoff,
   after which one probe job decides. Pausing Codex stops new coders; it does
   not route them to Claude.
3. **Soft throttles, labelled as such.** Optional per-window token ceilings that
   slow new dispatch. They are throttles, never claims that an allowance is
   exhausted.
4. **Per-job budget.** A profile may set `token_budget`. The bridge stops a job
   that passes it and marks it `halted` with reason `budget`, distinct from a
   provider `usage_limit`. Wall-clock limits already work this way.
5. **Claude pause behavior.** When Claude is limited, document, EPM and review
   work waits. It does not fall back to Codex for judgment-heavy planning
   unless the user approves that per request.
6. **GPU gating.** Local queue dispatch checks free VRAM and active leases.
7. **Visibility.** `watch` shows per-provider slot use, pause state with
   `deferred_until`, and the reason, using the existing halt classification.

## Relation to GUP work

Claude has suggested starting GUP revenue work. The user said this routing
system should precede it. This document does not decide how much routing must
exist first, and it adds no gate of its own:

- Whether GUP work waits for any milestone below, and which one, is the user's
  decision (Q10). Until the user answers, this draft neither blocks nor starts
  GUP work.
- GUP work, when it starts, goes through these routes like any other work:
  Codex master, Claude EPM, fresh Codex coders.
- This design does not read, change, deploy or publish anything in GUP, and it
  adds no financial or publishing automation.

## Incremental milestones

Each milestone is one or more scoped worker jobs with non-overlapping ownership.
Common checks for any code milestone: `gofmt -l .` prints nothing, `go vet
./...` and `go test ./...` pass, and the version file is bumped with a matching
tag by the integrator. Test names below are proposed names for the worker to
create.

**M0: decisions and guide update (documents only).**
Guide update: **done.** `~/AGENTS.md` now says (a) the Codex master may edit code
itself only for a task under one minute, (b) Claude instances are EPMs from
their first turn and never code, and (c) coding workers are Codex only. The
phrase `may choose Codex or Claude` no longer appears in it.
Remaining deliverable: the user's answers to Q1 to Q6, Q10 and Q15 recorded in
this file. The cron (Q1 to Q3), voice (Q8), GUP timing (Q10) and trial-default
questions are still open; the guide edit does not answer them.
Acceptance: `grep -nE '^- Q(1|2|3|4|5|6|10|15)\.' docs/ROUTING.md` prints
nothing (those questions moved to a dated "Decisions" section with the user's
words).

**M1a: Codex-only coder launches (landed, v0.20.1).**
`validAgentRole` rejects a Claude coder in requests, hard rules, verdicts, user
approvals and direct `start`; the Codex-to-Claude substitution is removed; a
limited-Codex coder request waits as `queued_usage_limit`. Covered by the tests
added in v0.20.1 in `launcher_test.go` and `main_test.go`. Do not redo.

**M1b: Claude defaults to EPM (landed, v0.20.2).**
`start --agent claude` selects role `epm` and profile `claude-epm` when neither
is given; `--agent claude --role coder` still fails; help text states the
defaults. Covered by `TestStartRole`, `TestStartClaudeDefaultsToEPM` and
`TestStartRejectsClaudeCoder`. Do not redo.

**M1c: EPM write scope and audit (code, launcher, Claude tool args).**
Closes G3 and layers 4, 7 and 8 above. Acceptance:
- `go test ./... -run 'TestEPMWriteScope|TestDocJobRejectsCodePaths|TestClaudeJobViolationAudit'` passes.
- A Claude EPM test job asked to write `main.go` is denied; writing under
  `docs/` is allowed.
- A Claude job whose worktree has a changed non-document path ends as
  `violation` and `agent-bridge show` reports it.
- A coder request while Codex is exhausted stays queued and no `claude-*` job is
  created (regression guard for v0.20.1).

**M1d: sub-minute exception record (documents; code only if the user wants
enforcement).** After Q15, add the evidence format to the status line and the
guide. No bridge change is proposed, because the bridge cannot see the master's
edits.

**M2: queue visibility and fairness (code, launcher, monitor, watch).**
Acceptance:
- `go test ./... -run 'TestQueuePerProviderFIFO|TestQueueNoHeadOfLineBlocking|TestQueueCancel|TestQueueExpiry'` passes.
- With the Claude cap full and a Codex request queued behind a Claude request,
  the Codex job dispatches.
- `agent-bridge latest | jq '.queue'` shows depth, oldest age and reasons;
  `agent-bridge watch --once` prints a queue section.
- `agent-bridge cancel ID` leaves `request-status ID` as `canceled`.

**M3: backpressure (code, launcher, profiles, monitor).**
Acceptance:
- `go test ./... -run 'TestProviderPause|TestDeferredUntilFromHalt|TestTokenBudgetHalt|TestCodexCap'` passes.
- A fixture halt for Codex defers new Codex coders with `defer_reason:
  provider_limit` and a `deferred_until`; after that time a probe job runs.
- A job that exceeds `token_budget` ends `halted` with reason `budget`, and
  partial work stays in its worktree.
- No test or run path ever changes a coder's agent to Claude.

**M4: routes and schedule registry (design plus code, gated on Q1 to Q3).**
Deliverable: a `route` field (`launch/v2`), the R-doc path (a Claude EPM whose `owns` are documents only), and the
schedule registry with the cron semantics the user chose.
Acceptance:
- `go test ./... -run 'TestRouteValidation|TestDocRouteCodePathRejected|TestScheduleRegistry'` passes.
- A request with a route that conflicts with its role is rejected with a stable
  rule string.
- Installing or changing a schedule entry requires user approval and appears in
  `approvals`.
- The scheduled job runs with no model call, and its exit status and log are
  readable through `agent-bridge show`.

**M5a: measured local trial, first slice (code, new files only).**
Prioritized by the user's latest instruction (objective 11). A durable, serial
local queue with objective telemetry and no subjective grading: `local-queue
submit`, `run`, `status`, `cancel` and `report`, finite retries, timeouts, and
automatic validity checks (output parses, label in the allowed set, output size
limit). Scoped task: `/tmp/agent-bridge-local-feedback-code-task.md`; the design
is `LOCAL-FEEDBACK.md`, milestone A. Acceptance:
- `go test ./... -run 'TestLocalQueue'` passes (serial order, retry limit,
  timeout, cancel, restart recovery, no raw prompt in telemetry by default).
- `agent-bridge local-queue status` shows depth, the running task and the oldest
  age; `local-queue report` prints per-class counts, valid rate and wall-time
  percentiles from telemetry.
- Telemetry never contains prompt or output text unless a debug flag is set.

**M5b: frontier review and routing report (code and documents).**
`LOCAL-FEEDBACK.md` milestones B and C: review grades and rescue cost recorded by
a frontier reviewer, a baseline and holdout for each class, and a report that
proposes threshold changes. Frontier agents apply a proposal; the bridge never
changes a threshold on its own.

**M5c: decision-tree playbooks and L3 execution (code).**
`LOCAL-FEEDBACK.md` milestones D and E, plus the original queue-and-playbook
scope. Acceptance:
- `go test ./... -run 'TestLocalLabelValidation|TestLocalGPUYieldPause|TestLocalCannotWriteCode'` passes.
- `agent-bridge local-model yield -- sleep 5` pauses the local queue with reason
  `gpu_yield` and resumes it afterwards.
- A model label outside the allowed set is rejected and retried, then escalated.
- Local rows show tokens in `watch`, and `token-feed.tsv` still parses for the
  Orbital Drift consumer or is gated by a feed version bump.

**M6: tool approval (L2, code).** Already planned in `LAUNCHER.md`; schedule it
before M1c if Q9 shows the Claude CLI cannot limit Edit and Write by path.
Acceptance: as in `LAUNCHER.md` (allow, deny and ask paths, with a deny on
approval-socket failure).

Proposed order: M1b (landed), then **M5a** (the measured local
trial; it touches only new files, so it can run beside M0 and M1c without
overlap), M0 (the guide edit is done; the user's answers remain), M5b once M5a has produced
real records, M1c (with M6 first if needed), M2, M3, then M4 as the user's
answers allow and M5c once at least one class has passed its holdout. The local
model should not stay at L3 "later": the trial is cheap, needs no new user
decision, and produces the data that tells us what L3 should contain. M4 stays
gated on Q1 to Q3. Where GUP work fits in this order is the user's decision
(Q10); this order does not decide it.

## Risks

| Risk | Mitigation |
| --- | --- |
| Prompt-only limits on Claude EPMs let code slip through (G3 is open) | Layers 4, 7 and 8 above; the audit blocks integration |
| The sub-minute exception grows into routine master coding | Honor-system today (G13); the master states estimate and elapsed time; the user can drop the exception (Q15) |
| Claude becomes the EPM default but tool access still allows edits | Default role change is a labelling fix, not enforcement; M1c is the enforcement |
| Removing Claude coding makes work wait when Codex is limited | Deferral with visible reason and retry time; the user may change the rule |
| Local model mislabels a branch | Enumerated labels, bounded retries, escalation, no file or shell tools |
| Local trial work is graded by the same frontier agent that routes it, and thresholds overfit | Objective validity checks first, a baseline and a holdout per class, thresholds changed only by an agent with a recorded report (`LOCAL-FEEDBACK.md`) |
| Local trial stores sensitive prompts | Telemetry holds sizes, hashes, counts and labels, not text, by default; raw capture is off and time-limited |
| Classifier or local verdict sends work to an expensive route | Model may only go cheaper within a route; ceilings stay deterministic |
| Queue grows silently | Queue section in snapshot and `watch`; ttl and expiry |
| Head-of-line fix reorders work | Per-provider FIFO is explicit and tested |
| Shared GPU between local queue and renders | Lease and yield pause; VRAM check |
| Schedule entries are system configuration | User approval for install or change; versioned registry; no model in the run path |
| Token totals mistaken for exhausted allowance | Only halt evidence marks a provider limited; totals are throttles |
| Escalations reach the user through non-Codex channels | Q7 |
| Document-only classification by path is too crude | Allowlist is configurable; audit failure is blocking, not silent |

## Open questions

Pending user clarification. None of these is assumed answered.

- Q1. What is a simple task for the cron route (single command, no output
  branching, time limit, no network)?
- Q2. What is recurring: a fixed schedule only, or also event-triggered?
- Q3. Cron means user crontab, a systemd user timer, or something else? Who
  approves installing an entry?
- Q4. What rubric chooses a Claude EPM versus a local EPM, and who decides when
  the call is close?
- Q5. Does a local EPM run only playbooks, or may it plan freely (L3 in
  `LAUNCHER.md`)?
- Q6. By "schedules" for Claude, do you mean planning documents, calendar items,
  or both? Calendar and mail connectors need authorization and are out of scope
  until you say otherwise.
- Q7. Should approval escalations reach you only through Codex, instead of the
  `approvals` CLI and desktop notifications?
- Q8. Voice (scope question only): is the voice daemon in scope for "Codex is
  the only bot I talk to"? Today it defaults to `--agent claude` with work
  access, and `--agent codex` is read-only. This draft makes no voice design
  decision.
- Q9. Does the Claude CLI permission system support path-limited Edit and Write
  for a headless job? If not, L2 must come before M1c. This needs a check by a
  worker, not an assumption.
- Q10. Should GUP revenue work wait for any routing milestone, and if so which
  one? No gate is proposed here; this is your call.
- Q11. Default numbers for the Codex cap, throttle windows and `token_budget`,
  and where to read your real plan limits.
- Q12. `claude-quick` and `claude-std` can no longer hold a coder. Should they be
  retired, renamed, or kept as cheaper EPM or documentation profiles?
- Q13. May a Claude EPM fall back to Codex for planning when Claude is limited,
  or must it wait?
- Q14. Is it acceptable that integration remains with the Codex master, as in
  `LAUNCHER.md` today?
- Q15. The under-one-minute exception: who estimates the minute, does it include
  checks, the version bump and the commit, and does it cover every non-document
  path or only small edits to existing files? Should the master record the
  estimate and elapsed time in its status line?
- Q16. "Right off the bat" for Claude: does every Claude entry point (`start`,
  launcher requests, the voice daemon, an interactive Claude session) start as
  an EPM? `start` and launcher requests do as of v0.20.2; the others are
  unchanged.
- Q17. Local trial defaults (proposed values are in `LOCAL-FEEDBACK.md`): the
  holdout share, the retention period for telemetry, whether raw prompt capture
  may ever be switched on, and whether the trial may run while a GPU render
  holds a lease (the proposal is no: the queue waits).
