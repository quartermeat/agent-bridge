# NUL-delimited: kind, id, provider, role, task, cwd, state, launcher, host pid, target kind, target name.
def field($value): ($value | tostring | if contains("\u0000") then error("NUL field") else . end), "\u0000";
def row($kind; $id; $provider; $role; $task; $cwd; $state; $launcher; $pid; $target_kind; $target_name):
  field($kind), field($id), field($provider), field($role), field($task), field($cwd), field($state), field($launcher), field($pid), field($target_kind), field($target_name);
if length != 1 or (.[0] | type) != "object" then error("expected one snapshot") else .[0] end
| if .version != 1 or (.jobs | type) != "array" or (.sources | type) != "array" then error("invalid snapshot schema") else . end
| (any(.sources[]; .name == "bridge-jobs" and .available == true)) as $healthy
| (any(.sources[]; .name == "activity-hooks" and .available == true)) as $hooks
| [.jobs[] | select(.status == "running" or .status == "starting")] as $active_jobs
| row("health"; ""; ""; ""; ""; ""; (if $healthy then "available" else "unavailable" end); ""; ""; ""; ""),
  (if $healthy then
    ($active_jobs[]
      | if (.id | type) != "string" or (.cwd | type) != "string" or (.name | type) != "string"
           or any([.id, .cwd, .name][]; contains("\u0000")) then error("invalid job") else . end
      | row("job"; .id; (.agent // "codex"); (.role // "coder"); .name; .cwd; .status; (.launcher // ""); (.worker_pid // ""); ""; "")),
    (if $hooks then
      (.agents // [])[]
      | select((.provider == "codex" or .provider == "claude")
          and (.activity_state as $state | ["active","running_job","has_running_children","approval_required","ready","halted_usage","error"] | index($state))
          and (.id | type) == "string" and (.cwd | type) == "string" and (.cwd | startswith("/")))
      | select(.id | startswith("local-") | not)
      | select(. as $agent | (any($active_jobs[];
          .session_id != null and .session_id != "" and .session_id == $agent.session_id) | not))
      | row("hook"; .id; .provider; "epm"; (.task // (.cwd | split("/") | last)); .cwd; .activity_state; ""; (.host_pid // ""); (.target.kind // ""); (.target.name // ""))
    else empty end)
  else empty end)
