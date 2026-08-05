#!/usr/bin/env bash
# Scriptable checks for the interactive Herdr decision-page walkthrough.
#
# The walkthrough creates the panes, starts Watchtower, answers the decision,
# and calls this script after each boundary. The script only observes the TUI
# and generated files; it never answers a decision or mutates the worktree.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  DATA_DIR=/path/to/data ISSUE_ID=GH-42 TUI_PANE=<pane> \
    scripts/e2e-decision-page.sh wait-decision
  DATA_DIR=/path/to/data ISSUE_ID=GH-42 \
    scripts/e2e-decision-page.sh check-boundary [previous_mtime] [expected_done]
  DATA_DIR=/path/to/data ISSUE_ID=GH-42 \
    scripts/e2e-decision-page.sh summary [expected_done]

Environment:
  DATA_DIR          Watchtower data directory (required).
  ISSUE_ID          Lane issue ID (required).
  TUI_PANE          Herdr pane to inspect (required by wait-decision).
  HERDR_BIN         Herdr executable (default: herdr).
  DECISION_PATTERN  TUI text indicating a pending decision (default:
                    waiting_decision).
  TIMEOUT_SECONDS   Poll timeout (default: 120).
  POLL_SECONDS      Poll interval (default: 1).
EOF
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

ok() {
  printf 'OK: %s\n' "$*"
}

DATA_DIR=${DATA_DIR:-}
ISSUE_ID=${ISSUE_ID:-}
HERDR_BIN=${HERDR_BIN:-herdr}
DECISION_PATTERN=${DECISION_PATTERN:-waiting_decision}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-120}
POLL_SECONDS=${POLL_SECONDS:-1}

[[ -n "$DATA_DIR" ]] || { usage >&2; fail "DATA_DIR is required"; }
[[ -n "$ISSUE_ID" ]] || { usage >&2; fail "ISSUE_ID is required"; }
[[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ && "$TIMEOUT_SECONDS" -gt 0 ]] || fail "TIMEOUT_SECONDS must be a positive integer"
[[ "$POLL_SECONDS" =~ ^[0-9]+$ && "$POLL_SECONDS" -gt 0 ]] || fail "POLL_SECONDS must be a positive integer"

ISSUE_DIR="$DATA_DIR/$ISSUE_ID"
DECISIONS_DIR="$ISSUE_DIR/decisions"
STABLE_PAGE="$ISSUE_DIR/decision.html"

mtime() {
  if stat -f %m "$1" >/dev/null 2>&1; then
    stat -f %m "$1"
  else
    stat -c %Y "$1"
  fi
}

page_paths() {
  local page
  shopt -s nullglob
  for page in "$DECISIONS_DIR"/*.html; do
    printf '%s\n' "$page"
  done
  shopt -u nullglob
}

page_count() {
  page_paths | wc -l | tr -d ' '
}

assert_page_shape() {
  local page=$1
  [[ -s "$page" ]] || fail "page is empty: $page"
  rg -q '<!doctype html>' "$page" || fail "missing HTML doctype: $page"
  rg -q 'id="decision"' "$page" || fail "missing decision anchor: $page"
  ! rg -qi '<script\b|javascript:|<foreignObject\b|<iframe\b|<link\b|https?://' "$page" || \
    fail "page is not self-contained or contains executable markup: $page"
}

assert_pages() {
  local pages count page
  [[ -d "$DECISIONS_DIR" ]] || fail "decision directory does not exist: $DECISIONS_DIR"
  count=$(page_count)
  [[ "$count" -gt 0 ]] || fail "no per-decision HTML page exists in $DECISIONS_DIR"
  [[ -s "$STABLE_PAGE" ]] || fail "stable decision page does not exist: $STABLE_PAGE"
  assert_page_shape "$STABLE_PAGE"
  while IFS= read -r page; do
    assert_page_shape "$page"
  done < <(page_paths)
  ok "$count per-decision page(s) and stable decision.html are present"
}

count_matches() {
  local pattern=$1
  local page=$2
  local count
  count=$(rg -o "$pattern" "$page" | wc -l | tr -d ' ' || true)
  printf '%s' "$count"
}

assert_floor_state() {
  local expected_done=${1:-}
  local done current pending
  done=$(count_matches 'class="pill done"' "$STABLE_PAGE")
  current=$(count_matches 'class="floor current"' "$STABLE_PAGE")
  pending=$(count_matches 'class="floor pending"' "$STABLE_PAGE")
  [[ "$current" -eq 1 ]] || fail "expected exactly one current floor, found $current"
  if [[ -n "$expected_done" ]]; then
    [[ "$expected_done" =~ ^[0-9]+$ ]] || fail "expected_done must be a non-negative integer"
    [[ "$done" -eq "$expected_done" ]] || fail "expected $expected_done done floor(s), found $done"
  fi
  ok "floor state: done=$done current=$current pending=$pending"
}

wait_for_decision_page() {
  local deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
  while (( $(date +%s) < deadline )); do
    if [[ -s "$STABLE_PAGE" ]] && [[ "$(page_count)" -gt 0 ]]; then
      assert_pages
      return 0
    fi
    sleep "$POLL_SECONDS"
  done
  fail "timed out waiting for generated decision pages"
}

wait_for_decision_state() {
  local pane=$1
  local deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
  command -v "$HERDR_BIN" >/dev/null 2>&1 || fail "Herdr executable not found: $HERDR_BIN"
  while (( $(date +%s) < deadline )); do
    local output
    if output=$("$HERDR_BIN" pane read "$pane" 2>/dev/null); then
      if rg -Fq "$DECISION_PATTERN" <<<"$output"; then
        ok "TUI pane $pane reports $DECISION_PATTERN"
        return 0
      fi
    fi
    sleep "$POLL_SECONDS"
  done
  fail "timed out waiting for $DECISION_PATTERN in TUI pane $pane"
}

wait_for_boundary() {
  local previous_mtime=$1
  local deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
  [[ "$previous_mtime" =~ ^[0-9]+$ ]] || fail "previous_mtime must be a non-negative integer"
  while (( $(date +%s) < deadline )); do
    if [[ -s "$STABLE_PAGE" ]]; then
      local current_mtime
      current_mtime=$(mtime "$STABLE_PAGE")
      if [[ "$current_mtime" -gt "$previous_mtime" ]]; then
        ok "decision.html mtime advanced from $previous_mtime to $current_mtime"
        return 0
      fi
    fi
    sleep "$POLL_SECONDS"
  done
  fail "timed out waiting for decision.html mtime to advance beyond $previous_mtime"
}

summary() {
  assert_pages
  assert_floor_state "${1:-}"
  printf 'SUMMARY: issue=%s stable_mtime=%s decision_pages=%s\n' \
    "$ISSUE_ID" "$(mtime "$STABLE_PAGE")" "$(page_count)"
}

case "${1:-}" in
  wait-decision)
    TUI_PANE=${TUI_PANE:-}
    [[ -n "$TUI_PANE" ]] || { usage >&2; fail "TUI_PANE is required for wait-decision"; }
    wait_for_decision_state "$TUI_PANE"
    wait_for_decision_page
    assert_floor_state "${2:-}"
    printf 'SUMMARY: issue=%s stable_mtime=%s decision_pages=%s\n' \
      "$ISSUE_ID" "$(mtime "$STABLE_PAGE")" "$(page_count)"
    ;;
  check-boundary)
    [[ $# -ge 2 ]] || { usage >&2; fail "check-boundary requires previous_mtime"; }
    wait_for_boundary "$2"
    assert_pages
    assert_floor_state "${3:-}"
    printf 'SUMMARY: issue=%s stable_mtime=%s decision_pages=%s\n' \
      "$ISSUE_ID" "$(mtime "$STABLE_PAGE")" "$(page_count)"
    ;;
  summary)
    summary "${2:-}"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
