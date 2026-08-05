#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-coach-exhausted"}'
read _task
for _attempt in 1 2 3; do
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0}}"}]}}'
  echo '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
  read _coach || exit 0
done
