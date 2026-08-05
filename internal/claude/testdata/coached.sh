#!/bin/sh
# Emits two incomplete decisions, then a complete cited-proof decision.
# Runner replies arrive only between turns, after each result.
echo '{"type":"system","subtype":"init","session_id":"s-coach"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": []}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
read coach
case "$coach" in
  *"missing required"*)
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": [], \"why\": \"a is standard\", \"consequences\": [\"done now\", \"more work\"], \"reversible\": \"anytime\", \"briefing\": {\"proof\": [{\"claim\": \"tests pass\"}]}}}"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"no coaching received"}]}}' ;;
esac
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
read coach
case "$coach" in
  *"missing required"*)
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": [], \"why\": \"a is standard\", \"consequences\": [\"done now\", \"more work\"], \"reversible\": \"anytime\", \"briefing\": {\"proof\": [{\"claim\": \"tests pass\", \"cite\": \"go test ./internal/decisionpage\"}]}}}"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"no second coaching received"}]}}' ;;
esac
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
read reply
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"proceeding"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
