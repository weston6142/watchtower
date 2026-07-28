#!/bin/sh
# Prose, then a tool-only message, then a result. The tool-only message is the
# case that used to write a blank transcript line.
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-tools"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"looking at the engine"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
