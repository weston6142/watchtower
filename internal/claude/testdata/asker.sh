#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-ask"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"guildhall_decision\": {\"question\": \"Pick one\", \"options\": [\"a\",\"b\"], \"recommended\": 1, \"importance\": 0.7, \"paths\": []}}"}]}}'
# Wait for the reply line on stdin (the initial task line arrives first).
read _first_line
read reply
case "$reply" in
  *"Human decision: b"*) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got b"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got other"}]}}' ;;
esac
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
