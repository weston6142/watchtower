#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-ask"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Pick one\", \"options\": [\"a\",\"b\"], \"recommended\": 1, \"importance\": 0.7, \"paths\": [], \"why\": \"b is clearer\", \"consequences\": [\"use a\", \"use b\"], \"reversible\": \"before implementation\"}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
# The reply arrives only after this turn's result; it starts a new turn.
read reply
case "$reply" in
  *"Human decision: b"*) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got b"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got other"}]}}' ;;
esac
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
