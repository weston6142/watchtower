#!/bin/sh
# Captures the first stdin line for TestTaskMessageMatchesRunnerInput, then
# emits a minimal session. `head -n 1` must come before any stdout write: the
# runner writes the task line before it reads stdout, and a `cat > file` here
# would block until stdin closes — which the runner only does after it sees a
# result event.
head -n 1 > "$TEST_WORKDIR/stdin.jsonl"
echo '{"type":"system","subtype":"init","session_id":"s-stdin"}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
