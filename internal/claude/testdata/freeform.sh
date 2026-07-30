#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-freeform"}'
read _task
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\":{\"kind\":\"freeform\",\"question\":\"Review spec.md\",\"recommended_response\":\"Approve spec.md as written.\",\"importance\":0.8,\"why\":\"It matches the design.\",\"consequences\":[\"Planning begins.\"],\"reversible\":\"yes\"}}"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
read reply
printf '%s' "$reply" > decision-reply.txt
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"feedback received"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
