#!/usr/bin/env bash
# Render every TUI flow from fixtures to ANSI + PNG for visual inspection.
# Usage: scripts/snap.sh [flow] [width]
set -euo pipefail
cd "$(dirname "$0")/.."
out=tmp-snaps
go run ./cmd/watchtower snap --out "$out" ${1:+--flow "$1"} ${2:+--width "$2"}
if command -v freeze >/dev/null; then
  for f in "$out"/*.txt; do
    freeze "$f" --output "${f%.txt}.png"
    echo "${f%.txt}.png"
  done
else
  echo "freeze not installed — ANSI dumps only" >&2
fi
