#!/bin/sh
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-fail"}'
echo '{"type":"result","is_error":true,"usage":{"input_tokens":1,"output_tokens":1}}'
