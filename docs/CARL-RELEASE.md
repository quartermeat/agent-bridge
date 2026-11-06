# Carl release plan

Status: draft consumer direction, 2026-11-06. The owner's host harness is unsandboxed by choice; the consumer release plan remains open.

## Goal

Consumers download Carl from our repos, and run `carl`. On first run he sets himself up through the prompt: their local model first, then optionally their own Claude and Codex subscriptions.

## Packaging

1. Extract Carl into his own repo, `quartermeat/carl`. Remove his dependence on the home `AGENTS.md`, `agent-bridge` paths, and this machine. Ship his EPM role rules with him, and make agent-bridge consume Carl rather than contain him.
2. Decide the licence.
3. Publish Linux, macOS and Windows binaries through GitHub Releases, with SHA-256 checksums and signed artifacts. Offer a verified download path first; never make piping a script into a shell the only option.
4. `carl update` verifies the signature before replacing the binary.

## First-run setup, through the prompt

There is no separate wizard. The first `carl` finds no config and starts the conversation.

1. Detect Ollama on localhost. If missing, explain how to install it and wait.
2. List models, recommend one that fits the hardware and supports tools, and offer to pull it after confirmation.
3. Offer Claude and Codex, skippable. Detect whether each CLI is installed and logged in.
4. Save only non-secret choices to the user config directory.

## Subscription login: security rules

Carl drives the official login; he never handles the credential.

- **The setup code runs login, never the model.** `claude auth login` and `codex login` are a fixed, hardcoded action, offered after the user confirms. A model cannot trigger, alter or add arguments to them.
- **Hands off the terminal.** The official CLI runs attached directly to the user's terminal, with Carl's output capture off. Its input and output never reach the model, logs, history, or telemetry.
- **Carl stores no tokens.** Each CLI keeps its own credentials. Carl checks only logged-in status through the CLI, and never reads their credential files. Tools must refuse those paths, plus common secret locations such as `~/.ssh`, by default.
- **Secrets never go in chat.** If the user pastes a key or token, Carl refuses to send it to any model, redacts it from history, and tells them to rotate it.
- **Use subscriptions only as the vendors allow.** Carl calls the official CLIs; he does not extract their tokens or impersonate them. Check each vendor's terms for this use before release.
- **Say what leaves the machine.** Local chat stays local. Before the first cloud call, say which provider receives the conversation and the files read.
- **`carl logout` and `carl status`.** Logout calls the official logout; status shows what is connected.

## Safe defaults

- Consumer defaults for host access, command execution, and confirmations are an open decision. This owner's host build deliberately runs unsandboxed; interactive calls confirm unless `--yes` is set, and jobs require `--write`.
- Ollama is localhost only; warn on a remote `OLLAMA_HOST`.
- No telemetry by default.
- File contents and web text are data, never instructions.

## Open decisions

- Consumer sandbox and permission defaults.
- Licence and repo visibility.
- Which platforms ship first.
- Whether Carl may spawn Claude and Codex for consumers, or only talk to them, and what EPM means outside this machine.

## Prerequisites

Carl must first be reliable here: merged, live spawning tested, and queue overlap closed.
