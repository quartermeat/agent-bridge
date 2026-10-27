# Measured local-model trial: queue, telemetry and feedback loop

Status: proposal for review (2026-10-26). Nothing here is implemented. It
extends `ROUTING.md` (route R-local, gap G15, milestones M5a to M5c) and builds
on the local model service in `LAUNCHER.md` and `LAUNCHER-PROFILES.md`.

## Why

The user's instruction of 2026-10-26: "I need you frontier models to give more
love to the local, monitor performance, so you have a feedback loop of what kind
of tasks it can handle in an amount of time."

Today the local model (`qwen3-coder:30b` through Ollama) only issues launch
verdicts. Nobody sends it other work, and nobody records how it does. This
document designs the smallest loop that fixes that:

1. frontier agents (the Codex master, or a Claude EPM) send the local model
   bounded tasks;
2. the bridge records time, tokens and objective result validity for each;
3. a frontier reviewer grades a sample and records what the review cost;
4. a periodic report shows, per task class, what the model handled and how fast;
5. frontier agents use the report to widen or narrow what they send.

The role rules do not change. Codex stays the only user-facing bot, every Claude
instance is an EPM, and runnable code work goes to fresh Codex coders. The local
model writes no files and no code.

## Principles

- **Output that worked is not output the model was sure of.** Success is
  decided by objective checks and by review. The model's own confidence is
  recorded only to measure calibration. It never gates routing.
- **A local failure must cost little.** Local tasks have no side effects and no
  one waits on them without a deadline. A frontier agent can always do the task
  itself.
- **Measure before trusting.** A task class starts as a trial. It becomes
  trusted only with enough graded samples and a passing holdout.
- **Frontier agents keep the judgment.** The bridge counts and reports. It does
  not change routing thresholds by itself.
- **No raw sensitive text in the record.** Telemetry holds sizes, hashes,
  counts and labels.

## Terms

| Term | Meaning |
| --- | --- |
| Task class | A named kind of bounded task with an input limit, an output schema, a validator, a default time budget and a trust state |
| Declared budget | The wall time the submitter says it can wait for a useful answer (`budget_s`) |
| Actual wall time | Seconds the task spent running, excluding queue wait and GPU-yield pauses (`run_s`) |
| Tokens | Ollama prompt and output token counts, summed over attempts (`prompt_tokens`, `output_tokens`) |
| Validity | Result of objective, bridge-run checks on the output: parsed, schema, label in set, size limit, and correctness when a gold answer exists |
| Review grade | A frontier reviewer's judgment of a valid output: `A`, `B`, `C` or `F` |
| Rescue cost | Frontier time and tokens spent fixing or redoing the output after the local model finished |
| Outcome | The derived verdict for one task (see "Outcome") |
| Arm | Which measurement group the task belongs to: `tune`, `holdout` or `control` |

## Task classes

A class is only eligible if its output can be checked by something other than
the model. Starting classes (the first two are the M5a slice; the rest need
review grades and wait for later milestones):

| Class | Input | Output | Objective validity | Review |
| --- | --- | --- | --- | --- |
| `label` | text up to 16 KiB, a list of allowed labels | exactly one label | label is in the allowed set; matches the gold label when one is supplied | Not needed when gold exists, otherwise sampled |
| `extract` | text up to 16 KiB, a list of field names | JSON object with exactly those string fields | parses; field set matches; each value within a length limit; gold match when supplied | Sampled |
| `summarize` (later) | text up to 32 KiB, a word limit | plain text under the limit | word limit; no empty text | Always, until trusted |
| `consistency` (later) | two text snippets and a question | `yes`, `no` or `unclear`, plus up to three quoted spans | label valid; each quoted span occurs in the input | Sampled |
| `branch` (later) | playbook step with enumerated labels | one label | label valid | Per playbook |

Frontier-only, never queued locally: code authoring or editing, anything that
needs a file write, shell command or network, content from `~/private` or any
file the sharing policy refuses, secrets, sensitive input, publishing,
financial actions, and any task whose wrong answer is acted on without a check.
GUP work is not excluded as a whole: a bounded, nonsensitive GUP classification
or extraction task may be trialed locally when it has objective checks and a
frontier review, and it must not touch anything on the frontier-only list. This
is a description of what the trial allows, not authorization to start GUP work;
when GUP work starts remains the user's decision (Q10). Classes are added
by editing the class table, which is code and goes through a Codex coder.

Each class has these fields:

```yaml
class: label
input_max_bytes: 16384
default_budget_s: 60        # unmeasured starting guess; replaced by observed p90
max_attempts: 2
review: gold_or_sample      # always | gold_or_sample | sample
state: trial                # trial | trusted | frontier-only
```

The starting numbers are guesses. The first measurements replace them.

## Time budget

The submitter declares `budget_s`, the longest it will wait for a useful answer.
The queue enforces two limits:

- `timeout_s` per attempt, default equal to `budget_s`, never above a class
  maximum. An attempt over the limit is canceled and counts as `timeout`.
- A total `deadline_at` for the task. After it passes, the task is `expired`
  if still queued, or canceled if running.

Timing fields: `queue_wait_s` (waiting in line), `paused_s` (waiting for a GPU
lease to end, from `waitForModelLease`) and `run_s` (model time, all attempts).
Only `run_s` is compared to the budget. A task is `within_budget` when
`run_s <= budget_s`. Queue wait is reported separately so a long line is not
blamed on the model, and so frontier agents can see when the line itself is the
reason to use another route.

The class table is then read as "tasks of this class and size finish within N
seconds": the report shows p50 and p90 `run_s` per class and size tier.

## Telemetry record (`local-task/v1`)

One JSON line per finished task, appended to
`~/.local/state/agent-bridge/local-queue/telemetry.jsonl` (directory `0700`,
file `0600`). No prompt or output text.

```json
{
  "spec": "local-task/v1", "id": "lq-...", "class": "label",
  "template_version": 1, "model": "qwen3-coder:30b",
  "submitted_by": "codex-...", "submitted_role": "coordinator",
  "arm": "tune", "tier": "S",
  "created_at": "...", "started_at": "...", "finished_at": "...",
  "budget_s": 60, "timeout_s": 60, "attempts": 1,
  "queue_wait_s": 3.2, "paused_s": 0, "run_s": 11.4, "within_budget": true,
  "input_bytes": 2048, "input_hash": "...", "output_bytes": 12, "output_hash": "...",
  "prompt_tokens": 640, "output_tokens": 4,
  "state": "done",
  "validity": {"checked": ["parsed", "label_in_set"], "failed": [], "result": "valid"},
  "gold": true, "correct": true,
  "claimed_confidence": null,
  "review": null
}
```

After review, a second append-only record links to the task and carries the
grade (see "Review and rescue cost"). Records are never edited in place.
`input_hash` uses a per-install salt so low-entropy inputs cannot be guessed
from the hash. `tier` is the input size band (S under 2 KiB, M under 8 KiB, L
otherwise), so the report can find the largest input a class handles.

## Result, review and confidence are separate

| Axis | Who decides | Meaning | Used for routing? |
| --- | --- | --- | --- |
| Validity | The bridge, by objective checks | The output has the required form, and matches a gold answer if there is one | Yes |
| Review grade | A frontier reviewer | The output is correct and usable | Yes |
| Claimed confidence | The model | How sure it says it is | No. Only to measure whether it is calibrated |

Review grades:

| Grade | Meaning |
| --- | --- |
| `A` | Correct and usable as is |
| `B` | Usable after a small edit |
| `C` | Partly right; the reviewer had to rework it |
| `F` | Wrong, unusable, or redone from scratch |

The reviewer also records `rescue_s` (frontier seconds spent after the local
model finished, 0 for `A`), `rescue_tokens` when known, `redo` (true if the
frontier did the task again), and `harm` (true if a wrong output was acted on
before it was caught). Review notes are at most 200 characters and must not
contain pasted task content.

Where a class has a gold answer, validity already includes correctness and no
subjective grade is needed. Gold tasks are the cheapest source of trusted
measurements, so the first exploration tasks should have them.

## Outcome

Derived from the record, never entered by hand:

| Outcome | Condition |
| --- | --- |
| `success` | state `done`, validity `valid`, (correct if gold, or grade `A`/`B`), and within budget |
| `late_success` | as `success`, but `run_s` is over budget |
| `rescued` | valid or invalid output, grade `B` or `C` with a rescue cost recorded |
| `redone` | grade `F`, `redo`, or an invalid output the frontier replaced |
| `failed` | state `failed` or `timeout` with no usable output |
| `canceled` | canceled by the submitter or an operator |
| `expired` | passed its deadline in the queue |

A task that was valid but not yet reviewed is `unreviewed`. Reports count these
separately and never as successes in a class that requires review.

The cost view uses `frontier_est_s`, the submitter's estimate of how long it
would have taken the frontier to do the task directly. A class earns its place
when median (`run_s` + `rescue_s`) is well below `frontier_est_s`, or when it
frees the frontier for other work at an acceptable error rate. Without an
estimate, the report shows rescue cost only.

## Queue

A durable, serial queue under `~/.local/state/agent-bridge/local-queue/`:

```text
tasks/<id>/task.json     # request, state, attempt log (no output text after review)
tasks/<id>/input         # copy of the input, 0600, removed per the privacy rules
telemetry.jsonl          # finished-task records
reviews.jsonl            # review records
```

- **Serial.** One runner holds a lock file and runs one task at a time, oldest
  first. The pinned model serves one request at a time; this matches
  `ollamaCompletion`, which already serializes in process.
- **States.** `queued`, `running`, `done`, `failed`, `timeout`, `canceled`,
  `expired`. Every change is an atomic file rename plus one decision-log line.
- **Finite retries.** `max_attempts` (default 2, class limit 3). Retry only on an
  invalid output, with the failed check named in the next prompt. A timeout is
  retried only if total budget remains. No retry after cancel.
- **Timeouts.** Per attempt and per task, as above.
- **Cancellation.** `local-queue cancel ID` marks the task. A queued task is
  never started. A running task has its Ollama request canceled, which stops
  generation. The submitter or any frontier agent may cancel; the record keeps
  who did.
- **GPU yield.** Before each attempt the runner calls `waitForModelLease`. Time
  spent waiting goes to `paused_s`. A GPU render always wins over the trial.
- **Restart recovery.** On start, a `running` task whose runner has gone is marked
  interrupted and requeued if attempts remain, otherwise `failed`.
- **Visible status.** `local-queue status [--json]` lists depth, the running task,
  oldest age, and per-state counts. The model's activity already shows as a
  `local` row in `snapshot` and `watch`. Adding a queue section to the snapshot
  belongs to M2 (`ROUTING.md`) and is not required for the trial.

## Who chooses what to send

### Routing decision: local or frontier

For each candidate task the submitting agent walks this list in order and stops
at the first match:

1. Frontier-only list matches, or the task would change a file, run a command
   or take any external action: **frontier**.
2. No class fits: **frontier**. If the task is low stakes, reversible and
   checkable, the agent may instead propose a new class (exploration, below).
3. The class state is `frontier-only`: **frontier**.
4. The class p90 `run_s` for this tier is above the declared budget: **frontier**,
   unless the task is not time critical and the agent chooses to wait.
5. Estimated wait (queue depth times class p50) is more than the slack before
   the deadline: **frontier**.
6. The class is `trusted`: **local**, with the class review rate applied.
7. The class is `trial`: **local as a trial** (exploration or a control pair),
   with the review rate for trials.

Why local at all: the task is small and well formed; its output can be checked;
the local model is free in usage allowance and runs in parallel with frontier
work; and a miss costs only the time to redo it. Why frontier: the work needs
judgment, a tool, a write, a long input, or a result no one can verify cheaply.

### Safe fallback

- The submitter sets a deadline and keeps working. It never blocks on the queue.
- On `failed`, `timeout`, `canceled`, `expired`, invalid output or grade `F`, the
  frontier agent does the task itself. This is the default path, not an
  exception.
- A late local result is discarded for the task but still recorded and graded
  for the report.
- Nothing downstream runs on an unreviewed local output unless its class is
  `trusted` and the output passed its objective checks.

### Exploration: choosing trial tasks

Frontier agents choose trial tasks. They should:

- prefer work they would do anyway that is small, reversible and checkable, such
  as labeling a halt reason, sorting job logs into categories, extracting fields
  from a report, or checking two snippets agree;
- use gold tasks where possible: tasks whose answer the agent already knows or
  can compute, so correctness needs no grade;
- walk a difficulty ladder per class: after 5 consecutive successes at a tier,
  submit the next tier up (a larger input or a tighter budget); after a
  failure, step back one tier;
- cap exploration at a share of eligible work during the trial (proposed 20% for
  a trial class), so the frontier is not spending its allowance only on testing;
- never explore with a task that touches the frontier-only list or any private
  or sharing-restricted input.

The agent records a one-line reason with each submission (for example "exploring
extract at tier M"). The reason is operational text, not task content.

### Baseline, holdout and tuning

To keep the loop honest, every task gets an `arm` from a stable hash of its ID
at submit time. The submitter cannot choose it.

| Arm | Share (proposed) | What happens | Used for |
| --- | --- | --- | --- |
| `tune` | 70% | Runs locally and is graded | Deciding thresholds |
| `holdout` | 15% | Runs locally and is graded | Only checking a threshold change that `tune` suggested; never used to choose it |
| `control` | 15% | The frontier does the task itself; the local model also runs, in the background, for comparison | The baseline: frontier time, tokens, and agreement with local |

The shares are unmeasured proposals (Q17 in `ROUTING.md`). Limits of the design:
the arm is assigned only after an agent decides a task is eligible, so the data
describes eligible tasks, not all work, and the report says so.

## Adjusting thresholds from data

The bridge reports; frontier agents decide. `local-queue report` (milestone C)
prints, per class and tier, over the `tune` arm: tasks, valid rate, success rate
with a lower confidence bound, p50 and p90 `run_s`, within-budget rate, median
rescue cost, and the `control` comparison. It lists proposed state changes with
the evidence behind each.

Proposed rules (starting values, to be replaced by measured ones):

| Change | Rule |
| --- | --- |
| `trial` to `trusted` | At least 20 graded `tune` tasks in the class and tier; lower bound of success rate at least 0.85; p90 `run_s` within the budget; median rescue cost at most 20% of `frontier_est_s`; then at least 10 `holdout` tasks with success at least 0.80 |
| `trusted` to `trial` | Success below 0.70 over the last 10 reviewed tasks, or a failed holdout check |
| any to `frontier-only` | Any `harm: true` record, or two consecutive `F` grades |
| Tier widened | The next tier passes the same checks on its own samples |
| Budget changed | Set the class budget to the observed p90 plus a margin, only after 20 tasks |

Guards against overfitting:

- Promote slowly, demote quickly. One bad record can demote; promotion needs the
  full sample and the holdout.
- At most one threshold change per class per 20 new graded tasks.
- Evaluate by tier, not only by class average, so easy tasks do not hide hard
  failures.
- A change of model, model digest, prompt template version or class validator
  resets the class to `trial` and starts its counts again.
- Trusted classes still get sampled review (proposed 10%) and still feed
  `control` and `holdout`, so drift shows.
- Every change is a dated record with the data it was based on: class, rule,
  sample sizes, who decided. The proposal comes from `report`; a Codex master or
  Claude EPM applies it after reading the evidence. Applying is a configuration
  change, so it follows the existing role rules: milestone C adds
  `local-queue routing set`, which records who changed what and why. Until then
  class states live in the code's class table and change only through a Codex
  coder job.

The report goes to the user through Codex, on request. No schedule is assumed: the
cron recurrence policy is still open (Q1 to Q3).

## Review and rescue cost

Who reviews: the agent that submitted the task reviews its own sample, except
that a class's holdout and a random 10% of trusted-class outputs are reviewed by
a different agent where one exists, so a single reviewer's habits do not set the
standard. A Codex master or a Claude EPM may review; reviewing is not coding. A
reviewer records its grade with `local-queue review ID --grade A|B|C|F
[--rescue-s N] [--rescue-tokens N] [--redo] [--harm] [--note TEXT]`. Review
needs the output text, which is kept until review and then removed (next
section).

Rescue cost is what the frontier spent because local output was not enough: the
time to fix or redo, and the tokens. It is the number that decides whether a
class is worth routing, and it is reported even for classes that look accurate.

## Privacy

- Telemetry and review records hold no prompt or output text, only sizes, salted
  hashes, token counts, labels, check names, grades and short operational notes.
- The task directory holds the input and output while the task is open. The
  input is removed when the task reaches a final state. The output is removed
  after review, or after 7 days if never reviewed (proposed; Q17).
- Prefer a path reference over a copy when the input is a file the task may read.
  The path must be under `~/work` or `~/worktrees` and must pass the existing
  sharing policy. Nothing under `~/private` and no file on the refuse list is
  accepted.
- Raw text capture for debugging is off. If switched on for one task with
  `--keep-text`, it expires after 24 hours and the record says it was on.
- Directories are `0700` and files `0600`. Nothing leaves the machine: Ollama is
  local, and no telemetry is sent anywhere.

## Milestones

| | Slice | Needs subjective grading? |
| --- | --- | --- |
| A | Serial durable queue, finite retries, timeouts, cancel, status, objective telemetry, `report` of counts and wall-time percentiles. Classes `label` and `extract` with gold support. `/tmp/agent-bridge-local-feedback-code-task.md` | No |
| B | `local-queue review`, review records, outcome derivation, rescue cost in the report | Yes |
| C | Arms (`tune`, `holdout`, `control`), threshold proposals, `routing set` with a change record | Yes |
| D | A short procedure document (a user-approved addition to `~/AGENTS.md`, or a doc in this repo) telling the Codex master and EPMs when to submit, review and read the report; classes `summarize` and `consistency` | Yes |
| E | Decision-tree playbooks run by the bridge with the model choosing among enumerated labels (`ROUTING.md` M5c), plus the snapshot queue section (M2) | Per playbook |

Milestone A is deliberately free of subjective grading. It can be tested with a
fake completion function: order, retries, timeouts, cancel, recovery, validity
checks, and the absence of text in telemetry are all objective.

## What this does not decide

- Cron recurrence (Q1 to Q3), GUP timing (Q10), and voice scope (Q8) stay open.
  The loop is run by frontier agents on demand, not by a schedule.
- Whether a local EPM exists (Q5). The trial uses single-step tasks and needs
  neither a playbook engine nor L3.
- The proposed numbers: shares, sample sizes, thresholds, retention. They are
  starting values to be replaced by measurement.
