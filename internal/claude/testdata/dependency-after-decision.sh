#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-dependency-valid"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\":{\"question\":\"Add prerequisites?\",\"options\":[\"yes\",\"no\"],\"recommended\":0,\"importance\":1.0,\"paths\":[],\"why\":\"the API must land first\",\"consequences\":[\"wait for the API\",\"continue without it\"],\"reversible\":\"before restart\"}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
read _reply
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_dependency\":{\"depends_on\":[\" GH-2 \",\"GH-3\",\"GH-2\"]}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
