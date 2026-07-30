#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-dependency-invalid"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_dependency\":{\"depends_on\":[\"GH-2\"]}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
