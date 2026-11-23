#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
install_dir=$HOME/.local/share/agent-bridge-widget
state_dir=$HOME/.local/state/agent-bridge/widget
profile=$HOME/.bash_profile
marker='# agent-bridge-termux-widget:v1'
export PATH="$HOME/.local/bin:$PATH"
command -v gojq >/dev/null 2>&1 || command -v jq >/dev/null 2>&1 || { printf 'install gojq or jq first\n' >&2; exit 1; }
mkdir -p -- "$install_dir" "$HOME/.shortcuts" "$state_dir"
chmod 700 "$install_dir" "$state_dir"
# Install a verified job viewer from a supplied binary, PATH, or this checkout.
helper=${WIDGET_JOB_VIEW_BIN:-}
if [[ -z $helper ]] && command -v widget-job-view >/dev/null 2>&1; then helper=$(command -v widget-job-view); fi
if [[ -z $helper ]]; then
  command -v go >/dev/null 2>&1 || { printf "widget-job-view or Go is required\n" >&2; exit 1; }
  helper=$(mktemp "$state_dir/widget-job-view.XXXXXXXX")
  trap 'rm -f -- "$helper"' EXIT
  (cd "$source_dir/../.." && go build -o "$helper" ./cmd/widget-job-view)
fi
[[ -f $helper && -x $helper ]] || { printf "widget-job-view helper is not executable: %s\n" "$helper" >&2; exit 1; }
mkdir -p -- "$HOME/.local/bin"
if [[ $helper != "$HOME/.local/bin/widget-job-view" ]]; then
  install -m 700 "$helper" "$HOME/.local/bin/widget-job-view"
fi
install -m 700 "$source_dir/widget.sh" "$install_dir/widget.sh"
install -m 600 "$source_dir/snapshot.jq" "$install_dir/snapshot.jq"

# Bash login runs this marked command after the user's existing profile.
if [[ ! -e $profile ]]; then : > "$profile"; fi
if ! grep -Fqx "$marker" "$profile"; then
  printf '\n%s\nnohup %q watch > %q 2>&1 < /dev/null &\n' "$marker" "$install_dir/widget.sh" "$state_dir/watch.log" >> "$profile"
fi
"$install_dir/widget.sh" refresh
nohup "$install_dir/widget.sh" watch > "$state_dir/watch.log" 2>&1 < /dev/null &

if [[ ${1:-} == --boot ]]; then
  mkdir -p -- "$HOME/.termux/boot"
  boot=$HOME/.termux/boot/agent-bridge-widget
  if [[ -L $boot ]]; then
    printf 'refusing to replace symlink: %s\n' "$boot" >&2
    exit 1
  fi
  if [[ -e $boot ]] && ! grep -Fqx "$marker" "$boot"; then
    printf 'refusing to replace unowned boot script: %s\n' "$boot" >&2
    exit 1
  fi
  printf '#!/data/data/com.termux/files/usr/bin/bash\n%s\nnohup %q watch > /dev/null 2>&1 < /dev/null &\n' "$marker" "$install_dir/widget.sh" > "$boot"
  chmod 700 "$boot"
fi
printf 'Agent Bridge widget updater installed in %s\n' "$install_dir"
