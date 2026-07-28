#!/bin/sh
# Emits a v1 (incomplete) decision, expects a coaching message, then emits v2.
echo '{"type":"system","subtype":"init","session_id":"s-coach"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": []}}"}]}}'
read _task
read coach
case "$coach" in
  *"missing required fields"*)
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": [], \"why\": \"a is standard\", \"consequences\": [\"done now\", \"more work\"], \"reversible\": \"anytime\"}}"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"no coaching received"}]}}' ;;
esac
read reply
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"proceeding"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
