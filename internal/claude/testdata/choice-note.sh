#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-choice-note"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\":{\"kind\":\"choice\",\"question\":\"What retry limit?\",\"options\":[\"Three\",\"Four\"],\"recommended\":0,\"allow_freeform\":false,\"importance\":0.8,\"why\":\"Bounded retries.\",\"consequences\":[\"Three retries.\",\"Four retries.\"],\"reversible\":\"yes\"}}"}]}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
read reply
printf '%s' "$reply" > decision-reply.txt
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"feedback received"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
