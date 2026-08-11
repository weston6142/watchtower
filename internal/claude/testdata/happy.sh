#!/bin/sh
# Reads and discards stdin lines in background; emits a fixed session.
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-happy"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}'
echo 'spec content' > "$TEST_WORKDIR/spec.md"
echo '{"type":"result","is_error":false,"usage":{"input_tokens":200,"output_tokens":100}}'
