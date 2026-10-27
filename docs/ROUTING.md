# Task routing: requirements and design draft

Status: draft for review (2026-10-26). Nothing in this document is implemented
unless the "Existing behavior" section says so. Companion to `LAUNCHER.md` (L1
launcher and EPM design) and `LAUNCHER-PROFILES.md` (job profiles).

## Objective

The user's request of 2026-10-26, restated as requirements:

1. Codex is the only agent the user talks to.
2. Codex creates an EPM (engineering project manager) in Claude or in a local
   model, according to task complexity.
3. EPMs use only freshly spawned Codex instances for runnable code work, under
   the organizational rules in `~/AGENTS.md`.
4. Documents, requirements and schedules go to Claude.
5. Simple tasks become a cron job. What counts as simple, and what counts as
   recurring, is **pending user clarification**.
6. Tasks whose decision tree is larger than cron go to the local model, which
   needs a queue.
7. Spawned Claude and Codex jobs run within their token or usage limits.
8. This routing system precedes any new GUP revenue work.

Non-goals: autonomous publishing, financial transactions, GUP deployment, and
any change to GUP. This document does not touch GUP.

## Actors and boundaries

| Actor | Does | Must not |
| --- | --- | --- |
| User | Talks to the Codex master; resolves escalations; grants explicit approval for sensitive actions | n/a |
| Codex master (coordinator) | Classifies the task, writes the task spec, requests an EPM or a document job, reviews, integrates, commits and tags | Edit code, tests, data, build files, configuration, assets or version files itself |
| Claude EPM | Plans and splits work, writes documents and task prompts, requests fresh Codex coders, reviews reports and diffs | Write or edit code; run project code; integrate to `main` |
| Claude document job | Writes requirements, designs, schedules and other documents inside owned documentation paths | Touch any non-document path |
| Local model | Runs queued decision-tree tasks and launch verdicts; may act as a local EPM once L3 exists | Write files or run commands on its own; write code; pick Claude or Codex coders outside the rules |
| Codex coder | Fresh job per task, own worktree and branch, owned paths, exact checks | Spawn jobs; resume an earlier session; widen its owned paths |
| Launcher (`agent-bridge launcher`) | Applies deterministic rules, asks the local model for verdicts, creates worktrees, starts jobs | Be overridden by any model verdict |
| Monitor | Publishes snapshots of agents, jobs, halts, GPU and tokens | Make routing decisions |

Role boundary reminders from `~/AGENTS.md`: the master never codes; limits do not
move coding authority to the master; integration (commit, tag, merge) stays with
the master after reviewing the report, diff and checks; no push, publish or
deploy without explicit user intent.

"Fresh" Codex means a new bridge job per task: new job ID, new worktree and
branch, no reused or resumed session, and no context beyond the task spec and
the files passed with `--context`.

"Code" means every path that is not a document. For enforcement, a document is a
file under `docs/` or a Markdown file or a task prompt. Everything else is code,
including tests, data, build files, configuration, assets and version files,
matching the master role boundary in `~/AGENTS.md`.

## Route table

The Codex master picks a route and states it in the request. The launcher
validates the route against hard rules; the local model may only pick a cheaper
option inside the route, never a different route class.

| Route | Trigger (proposed rubric; pending confirmation) | Executor | Writes | Output |
| --- | --- | --- | --- | --- |
| R-cron: one-off or simple recurring | Single deterministic command or script, no branching on its output, no judgment. "Simple" and "recurring" thresholds are pending user clarification | A scheduled job spec that runs without any model. The script itself is code, so a fresh Codex coder writes it | Script and schedule entry, both code or configuration | Exit status and log |
| R-doc: documents, requirements, schedules | The deliverable is prose or a plan, no runnable code | Claude document job (or a Claude EPM that owns only documentation paths) | Documentation paths only | Document plus report v2 |
| R-local: decision tree larger than cron | Bounded steps with branches decided by classification, triage or checks; no code authoring | Local model through the local queue | Result files in the job state directory only | Structured result (JSON or YAML) plus log |
| R-code: coding | Any change to a non-document path | Claude EPM or local EPM plans and splits; each unit of work runs in a fresh Codex coder | Owned paths in the worker's own worktree | Branch, report v2, check output |
| R-review: review and integration | A finished worker branch | Fresh read-only Codex job for review when checks must run; the EPM reviews reports and diffs; the Codex master integrates | Integration commit and tag by the master only | Reviewed diff, evidence, commit |

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

Implemented as of v0.20.0 (L1, checked against `launcher.go`, `profiles.go`,
`main.go`, `localmodel.go` and the packaged units):

| Area | Behavior | Where |
| --- | --- | --- |
| Roles and depth | `coder` and `epm`; Codex cannot be `epm`; depth cap 3 | `launcher.go:126`, `launcher.go:540` |
| Requester rules | Coordinator may request only an `epm`; only a running Claude EPM job may request from a job; Codex and coder jobs cannot | `launcher.go:490`, `launcher.go:534` |
| Spool and CLI | `request`, `request-status`, `launcher`, `approvals`, `approve`, `deny`; private spool, FIFO by mtime | `launcher.go:340`, `launcher.go:866` |
| Local verdict | Ollama model returns approve, escalate or reject as validated JSON; timeouts and errors escalate | `launcher.go:583`, `launcher.go:646` |
| Dispatch | Worktree at `~/worktrees/<repo>-<task>` on `worker/<task>`, then `start --write` with parent job and spawner | `launcher.go:695` |
| Profiles | Seven named profiles, cost order, wall-clock limit enforced by the bridge | `profiles.go`, `packaging/profiles.yaml` |
| Concurrency | At most 4 live launcher jobs (`AGENT_BRIDGE_LAUNCHER_MAX_JOBS`), 2 Claude, 1 `claude-deep`; over-cap requests wait | `launcher.go:70` |
| Usage backpressure | Reactive: a provider is exhausted if an agent is `halted_usage` or a job halted with `usage_limit` in the last hour; launcher substitutes a profile or escalates | `launcher.go:99`, `launcher.go:111`, `launcher.go:801` |
| Escalation | Pending files, desktop notification, timeout deny | `launcher.go:690`, `launcher.go:837` |
| Observability | `decisions.jsonl`, job fields `agent`, `role`, `depth`, `profile`, `substitution`; snapshot and `watch` | `launcher.go:469` |
| Local model | Pinned model service, lease and yield around GPU renders, completions via `ollamaCompletion` | `localmodel.go` |

Planned, not implemented: **L2** (launcher approval socket for tool calls inside
Claude jobs) and **L3** (the local model executes small tasks itself). Neither
is relied on as existing behavior below; each gap that needs them says so.

Gaps against the objective:

| # | Gap | Evidence | Needed by |
| --- | --- | --- | --- |
| G1 | Claude can be a **coder**. The hard rules accept `agent: claude, role: coder`, the verdict parser accepts it, and profiles `claude-quick` and `claude-std` are described as implementation profiles | `launcher.go:610`, `LAUNCHER-PROFILES.md` | M1 |
| G2 | **Cross-provider substitution for coding.** When Codex is exhausted, `codex-std` becomes `claude-quick` and `codex-deep` becomes `claude-std`, so a coding task moves to Claude | `launcher.go:99` | M1 |
| G3 | **EPMs can edit code.** The launcher always passes `--write`, and Claude write jobs get Edit and Write; the only barrier is the prompt | `launcher.go:710`, `README.md` (`start`) | M1 (path-limited tools if the Claude CLI supports them, otherwise L2) |
| G4 | **No local EPM.** A coordinator may launch only a Claude EPM, and the local model cannot run multi-step work | `launcher.go:534`, `launcher.go:610` | M5 |
| G5 | **No route concept.** Requests carry role and agent, not a route; there is no document-only role, no cron path and no local task queue | `launchRequest` | M4, M5 |
| G6 | **Head-of-line blocking.** One request waiting on capacity stops the pass (`break`), so a Codex request behind a capped Claude request also waits | `launcher.go:900` | M2 |
| G7 | **Queue is invisible.** Spool and capacity waits are not in the snapshot, `list` or `watch`; there is no queue depth, age or deferral reason | `monitor.go`, `watch_render.go` have no spool reference | M2 |
| G8 | **No cancel, priority or expiry** for queued launch requests (pending approvals do expire) | `launcher.go:837` | M2 |
| G9 | **Backpressure is reactive and coarse.** No Codex cap, no deferral time, no per-job token budget; an exhausted provider is detected only after a job halts | `launcher.go:70` | M3 |
| G10 | **User still talks to more than Codex.** Escalations go to `approvals` and desktop notifications, and the voice daemon defaults to `--agent claude` with work access | `README.md` voice section | open question Q7, Q8 |
| G11 | `~/AGENTS.md` says the master may choose Codex or Claude for each job. The user's instruction narrows this for coding; the guide has not been updated | `~/AGENTS.md` | M0 (user-approved edit) |

## Strict Codex-only coding enforcement

Requirement: no code reaches a worktree except through a fresh Codex coder job.
Enforce in layers, because a prompt alone does not enforce anything.

1. **Request rule (deterministic).** `role: coder` requires `agent: codex`.
   `agent: auto` for a coder resolves to Codex. A violating request is rejected
   with a stable rule string, for example `coding requires Codex`, visible in
   `request-status`.
2. **Verdict rule.** The verdict parser rejects any model verdict that sets a
   coder to Claude, whatever the model returns.
3. **No substitution across providers for coding.** Remove the Codex-to-Claude
   entries from the substitution table. When Codex is limited, the coder request
   is deferred (see backpressure), never moved. Codex-to-Codex profile
   downgrades inside one provider remain allowed.
4. **Claude jobs get document-only write access.** Claude EPM and document jobs
   run with Edit and Write limited to the task's `owns` paths, and the launcher
   rejects any task whose `owns` includes a code path. The mechanism is
   path-limited tool permissions if the Claude CLI supports them; if it does
   not, the mechanism is the L2 approval socket, and M1 ships read-only EPM
   tools plus a document-job path until L2 exists. This must be verified, not
   assumed (open question Q9).
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
Claude to code.

## Queues and lifecycle

Three queues are needed. Only the first exists.

| Queue | Holds | Executor | Status |
| --- | --- | --- | --- |
| Launch queue | `launch/v1` requests for worker jobs | Launcher | Exists (spool) |
| Local queue | `local/v1` decision-tree tasks | Pinned local model, one at a time | Needed (M5) |
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

Whether a local EPM is in scope at all, or the local model should remain a
dispatcher and playbook runner, is the existing open question in `LAUNCHER.md`
(L3) and is repeated below.

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

Claude has suggested starting GUP revenue work. This routing system is to
precede it. Requirements for that ordering:

- No GUP task is created, scheduled or routed until the user accepts a named
  routing milestone. Proposed gate: M1 through M3 accepted (Codex-only coding,
  a visible fair queue, and backpressure). The user chooses the gate (Q10).
- GUP work, when it starts, goes through these routes like any other work.
- This design does not read, change, deploy or publish anything in GUP, and it
  adds no financial or publishing automation.

## Incremental milestones

Each milestone is one or more scoped worker jobs with non-overlapping ownership.
Common checks for any code milestone: `gofmt -l .` prints nothing, `go vet
./...` and `go test ./...` pass, and the version file is bumped with a matching
tag by the integrator. Test names below are proposed names for the worker to
create.

**M0: decisions and guide update (documents only).**
Deliverable: the user's answers to Q1 to Q6 and Q10 recorded in this file;
`~/AGENTS.md` edited (user-approved) to say coding workers are Codex-only for
EPM-managed work.
Acceptance: `grep -nE '^- Q(1|2|3|4|5|6|10)\.' docs/ROUTING.md` prints nothing
(those questions moved to a dated "Decisions" section with the user's words),
and `grep -n 'may choose Codex or Claude' ~/AGENTS.md` no longer describes
coding as a free choice.

**M1: Codex-only coding enforcement (code, launcher, profiles).**
Acceptance:
- `go test ./... -run 'TestCoderRequiresCodex|TestNoCrossProviderCodingSubstitution|TestVerdictRejectsClaudeCoder|TestEPMWriteScope|TestDocJobRejectsCodePaths'` passes.
- `agent-bridge request --file` with `role: coder, agent: claude` ends with
  `request-status` showing `rejected` and rule `coding requires Codex`.
- With a fake snapshot showing Codex `halted_usage`, a `codex-std` coder request
  is `deferred`, and no `claude-*` job is created.
- A Claude EPM test job asked to write `main.go` is denied; writing under
  `docs/` is allowed.
- A Claude job whose worktree has a changed non-document path ends as
  `violation` and `agent-bridge show` reports it.

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
Deliverable: a `route` field (`launch/v2`), the R-doc document job path, and the
schedule registry with the cron semantics the user chose.
Acceptance:
- `go test ./... -run 'TestRouteValidation|TestDocRouteCodePathRejected|TestScheduleRegistry'` passes.
- A request with a route that conflicts with its role is rejected with a stable
  rule string.
- Installing or changing a schedule entry requires user approval and appears in
  `approvals`.
- The scheduled job runs with no model call, and its exit status and log are
  readable through `agent-bridge show`.

**M5: local queue and playbooks (L3, code).**
Acceptance:
- `go test ./... -run 'TestLocalQueueSerial|TestLocalLabelValidation|TestLocalGPUYieldPause|TestLocalCannotWriteCode'` passes.
- `agent-bridge local-model yield -- sleep 5` pauses the local queue with reason
  `gpu_yield` and resumes it afterwards.
- A model label outside the allowed set is rejected and retried, then escalated.
- Local rows show tokens in `watch`, and `token-feed.tsv` still parses for the
  Orbital Drift consumer or is gated by a feed version bump.

**M6: tool approval (L2, code).** Already planned in `LAUNCHER.md`; schedule it
before M1 if Q9 shows the Claude CLI cannot limit Edit and Write by path.
Acceptance: as in `LAUNCHER.md` (allow, deny and ask paths, with a deny on
approval-socket failure).

Order: M0, M1 (with M6 first if needed), M2, M3, then M4 and M5 as the user's
answers allow. The proposed GUP gate is after M3.

## Risks

| Risk | Mitigation |
| --- | --- |
| Prompt-only limits on Claude EPMs let code slip through | Layers 4 to 7 above; the audit blocks integration |
| Removing Claude coding makes work wait when Codex is limited | Deferral with visible reason and retry time; the user may change the rule |
| Local model mislabels a branch | Enumerated labels, bounded retries, escalation, no file or shell tools |
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
  for a headless job? If not, L2 must come before M1. This needs a check by a
  worker, not an assumption.
- Q10. Which milestone gates the start of GUP revenue work? Proposed: M3.
- Q11. Default numbers for the Codex cap, throttle windows and `token_budget`,
  and where to read your real plan limits.
- Q12. Should `claude-quick` and `claude-std` be retired for coders and kept only
  for document jobs, or renamed?
- Q13. May a Claude EPM fall back to Codex for planning when Claude is limited,
  or must it wait?
- Q14. Is it acceptable that integration remains with the Codex master, as in
  `LAUNCHER.md` today?
