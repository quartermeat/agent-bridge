# Agent Bridge home-screen widget for Termux

The package creates background Termux:Widget tasks in `~/.shortcuts/tasks/`. Tapping a direct headless job row reads a fresh Agent Bridge snapshot and checks the exact job ID and `direct` launcher. It then calls `widget-job-view ID`. That helper checks authoritative job metadata and opens the worker's live user-facing output in Android's browser, reusing its view on repeated taps. The widget never copies event logs or renders HTML. It never changes `allow-external-apps`.

An interactive hook row can bring an **existing** native Termux terminal activity forward. The shortcut records the hook's host PID and Linux process start time at refresh. At tap time, it checks a fresh hook snapshot, the same PID lifetime, its Termux-owned executable and UID, controlling PTY, and ancestry to the `com.termux` app process. Only then does it send a normal MAIN/launcher intent for Termux. This does not send commands or create a terminal. Stock Termux 0.118.3 has no supported way to select a particular native tab, so on a multi-tab phone the user selects the tab from the drawer. An explicit target in the hook snapshot is retained during validation, but this package does not act on a tmux target: a background widget task has no verified existing tmux client to switch. Empty, stale, detached, non-native, and unverifiable hook targets do nothing.

With no running rows, the widget shows `Codex · No working agents`. If the bridge-jobs source or snapshot is unavailable, it shows `Codex · Status unavailable`. These labels are inert. Only active direct jobs receive job rows; idle hooks and other job launchers do not use the helper. `Refresh widget` remains a manual background task. The updater migrates its marked old foreground shortcuts, preserves user-owned shortcuts and handles label collisions by ID.

## Requirements and install

Install Termux:Widget and place its widget on the home screen. The package needs Bash, `flock`, coreutils, `agent-bridge`, and `gojq` or `jq`. Supply an executable `widget-job-view` with `WIDGET_JOB_VIEW_BIN=/path/to/widget-job-view`, put a prebuilt one on `PATH`, or have Go installed so setup builds `./cmd/widget-job-view` locally. Setup installs the helper to `~/.local/bin/widget-job-view`. The job helper uses Android's browser through `am`; no external file provider is needed.

```sh
WIDGET_JOB_VIEW_BIN=/path/to/widget-job-view ./packaging/termux-widget/setup.sh
```

Setup copies the updater and jq filter into `~/.local/share/agent-bridge-widget`, refreshes shortcuts, and starts a watcher. It adds one marked launch command to `~/.bash_profile`, preserving existing content. A lock prevents duplicate polling loops. Repeating setup is safe. Optional `--boot` installs a marked Termux:Boot script. The watcher log is `~/.local/state/agent-bridge/widget/watch.log`. The refresh broadcast needs Termux:Widget 0.114.0 or newer; older versions can use the widget's refresh button.

## Check

```sh
./packaging/termux-widget/test.sh
bash -n packaging/termux-widget/{widget,setup,test}.sh
```

Fixtures use a stub helper and Android activity command. They cover distinct IDs with identical labels, repeated taps, stale and empty targets, PID reuse, detached PTYs, migration, shortcut ownership, malformed snapshots, manual refresh, broadcast retry, and repeatable isolated setup. Tests do not run live setup.
