#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-coach-exhausted"}'
read _task
for _attempt in 1 2 3; do
  if [ "$_attempt" -eq 3 ]; then
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0}}"},{"type":"tool_use","name":"Read","input":{"file_path":"ISSUE.md"}}]}}'
  else
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0}}"}]}}'
  fi
  echo '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
  read _coach || exit 0
done
