#!/bin/bash
# Emulates the real CLI's steering semantics around a mid-turn decision:
# the agent emits a decision and keeps working in the same turn. A user
# message that arrives while the turn is in flight is absorbed into it
# (steering) and never starts a new turn; only a message that arrives
# while idle between turns does.
echo '{"type":"system","subtype":"init","session_id":"s-midturn"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Pick one\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.3, \"paths\": [], \"why\": \"a matches\", \"consequences\": [\"use a\", \"use b\"], \"reversible\": \"anytime\"}}"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"continuing with the work, not blocking on it"}]}}'
# Steering window: a reply sent while this turn is still running is
# swallowed here and no new turn will ever begin.
if read -t 2 _steered; then
  echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
  read -r _ # idle: waits for input that the runner will never send
  exit 0
fi
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
# Idle between turns: a message now starts a fresh turn.
if read -r _reply; then
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"acknowledged, already implemented per recommendation"}]}}'
  echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
fi
read -r _ # wait for EOF
exit 0
