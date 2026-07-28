#!/usr/bin/env bash
# Capture the REAL watchtower TUI in tmux against a seeded temp daemon.
# Usage: scripts/tui-capture.sh <floor|decision|tray|modal|levers|arch|help>
#
# Note: termenv degrades to monochrome under tmux, so live captures verify
# STRUCTURE against real daemon data; color fidelity is verified by the
# fixture snapshots (scripts/snap.sh), which pin a truecolor profile.
set -euo pipefail
cd "$(dirname "$0")/.."
flow="${1:-floor}"
sess="ghsnap-$$"
# Darwin limits unix socket paths; keep the data dir short.
dir="$(mktemp -d /tmp/ghcap.XXXXXX)"
repo="$dir/repo"
data="$dir/data"
out=tmp-snaps
mkdir -p "$out" "$repo"
bin="$dir/watchtower"
go build -o "$bin" ./cmd/watchtower

cleanup() {
  tmux kill-session -t "$sess" 2>/dev/null || true
  pkill -f "$bin" 2>/dev/null || true
}
trap cleanup EXIT

# Seed: init a workspace with a fake runner and create one issue (auto-spawns daemon).
(cd "$repo" && "$bin" init --data "$data" >/dev/null)
printf 'runner: fake\n' > "$repo/.watchtower/config.yaml"
(cd "$repo" && "$bin" new --data "$data" --title "create a repo for GH-1" >/dev/null)

tmux new-session -d -s "$sess" -x 200 -y 50 \
  "cd '$repo' && TERM=xterm-256color COLORTERM=truecolor CLICOLOR_FORCE=1 '$bin' tower --data '$data'"
tmux set-option -t "$sess" default-terminal "tmux-256color" 2>/dev/null || true

# Wait for first paint.
for _ in $(seq 1 50); do
  if tmux capture-pane -t "$sess" -p 2>/dev/null | grep -q '[^[:space:]]'; then
    break
  fi
  sleep 0.2
done

# Drive to the requested flow.
case "$flow" in
  floor) ;;
  decision) tmux send-keys -t "$sess" d ;;
  tray) tmux send-keys -t "$sess" t ;;
  modal) tmux send-keys -t "$sess" n ;;
  levers) tmux send-keys -t "$sess" L ;;
  arch) tmux send-keys -t "$sess" A ;;
  help) tmux send-keys -t "$sess" '?' ;;
  *) echo "unknown flow: $flow" >&2; exit 1 ;;
esac
sleep 0.5

tmux capture-pane -t "$sess" -p -e > "$out/live-$flow.txt"
if command -v freeze >/dev/null; then
  freeze "$out/live-$flow.txt" --output "$out/live-$flow.png"
  echo "$out/live-$flow.png"
else
  echo "$out/live-$flow.txt"
fi
