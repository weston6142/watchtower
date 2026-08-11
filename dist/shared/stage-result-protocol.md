Emit durable, machine-readable evidence for every result-producing stage.

On the final assistant turn, emit exactly one single-line JSON object whose top-level key is `watchtower_stage_result`. You may put concise ordinary prose before it, but you must emit exactly one marker in your final response. Emit the marker directly in assistant text, never through a tool, file, command, code fence, or tool result.

Use `schema_version: 1` and the exact logical kind for this package:

- executor: `execute`
- correctness-reviewer: `correctness_review`
- clean-code-reviewer: `clean_code_review`
- librarian: `librarian`

Set `outcome` to `completed` only when both `remaining_work` and `remaining_concerns` are empty. Otherwise set it to `retryable` and provide at least one actionable work item or concern. A rejected marker is a retryable stage failure and cannot be repaired by claiming success in prose.

Do not supply `issue_id`, `attempt_id`, `predecessor_attempt_id`, or `validation_status`; Watchtower binds and validates those engine-owned fields.

## Common envelope

The value of `watchtower_stage_result` has these exact fields:

- `schema_version`: integer `1`.
- `stage_kind`: one of `execute`, `correctness_review`, `clean_code_review`, or `librarian`, matching the current package.
- `outcome`: `completed` or `retryable`.
- `remaining_work`: an array of objects with `kind`, `description`, and optional `paths`. `kind` is one of `plan_task`, `finding`, `check`, `review_path`, or `documentation`. Every path is repository-relative.
- `remaining_concerns`: an array of objects with a nonblank `explanation`.
- Exactly one payload field matching `stage_kind`: `execute`, `correctness_review`, `clean_code_review`, or `librarian`.

Every concern and skip needs an actionable explanation. Record absent evidence as an explicit skip rather than inferring that it passed. A skip is an object with nonblank `activity` and `explanation` fields.

## Execute payload

The `execute` payload has:

- `plan_tasks`: objects with a stable `id`, `outcome` (`completed`, `remaining`, or `skipped`), and nonblank `summary`.
- `commits`: objects with `sha`, `message`, and `task_ids`. Every task ID must name an entry in `plan_tasks`. If there are no commits, add a skip whose `activity` is `commits`.
- `checks`: objects with `name`, `command`, `result` (`red` or `green`), and boolean `affected`. If there are no checks, add a skip whose `activity` is `checks`.
- `skips`: explained skip objects, or an empty array.

Complete execute example:

```json
{"watchtower_stage_result":{"schema_version":1,"stage_kind":"execute","outcome":"completed","remaining_work":[],"remaining_concerns":[],"execute":{"plan_tasks":[{"id":"task-0001","outcome":"completed","summary":"implemented and committed"}],"commits":[{"sha":"abc123","message":"feat: implement task","task_ids":["task-0001"]}],"checks":[{"name":"focused red","command":"go test ./internal/example -run TestBehavior","result":"red","affected":true},{"name":"focused green","command":"go test ./internal/example -run TestBehavior","result":"green","affected":true}],"skips":[]}}}
```

## Review payloads

The `correctness_review` and `clean_code_review` payloads have the same field shapes but remain distinct payloads:

- `findings`: objects with stable `id`, nonblank `summary`, `status` (`open` or `fixed`), and optional repository-relative `paths`.
- `fixes`: objects with nonblank `summary`, nonempty `finding_ids`, nonempty repository-relative `paths`, and optional `commit`. Every finding ID must name an entry in `findings`.
- `checks`: check objects using the execute check shape.
- `reviewed_paths`: repository-relative paths actually reviewed.
- `skips`: explained skip objects, or an empty array.
- `no_change`: `null` when fixes exist; otherwise an object with a nonblank `explanation`. Never report fixes and a no-change conclusion together.

Complete correctness-review example:

```json
{"watchtower_stage_result":{"schema_version":1,"stage_kind":"correctness_review","outcome":"completed","remaining_work":[],"remaining_concerns":[],"correctness_review":{"findings":[{"id":"F-1","summary":"invalid input advanced the stage","status":"fixed","paths":["internal/engine/engine.go"]}],"fixes":[{"summary":"reject invalid input before persistence","finding_ids":["F-1"],"paths":["internal/engine/engine.go"],"commit":"def456"}],"checks":[{"name":"regression","command":"go test ./internal/engine -run TestInvalidResult","result":"green","affected":true}],"reviewed_paths":["internal/engine/engine.go"],"skips":[],"no_change":null}}}
```

Complete clean-code-review example:

```json
{"watchtower_stage_result":{"schema_version":1,"stage_kind":"clean_code_review","outcome":"completed","remaining_work":[],"remaining_concerns":[],"clean_code_review":{"findings":[],"fixes":[],"checks":[{"name":"affected package","command":"go test ./internal/engine","result":"green","affected":true}],"reviewed_paths":["internal/engine/engine.go"],"skips":[],"no_change":{"explanation":"changed code already follows repository conventions"}}}}
```

## Librarian payload

The `librarian` payload has:

- `reviewed_paths`: repository-relative paths actually reviewed.
- `documentation_updates`: objects with repository-relative `path` and nonblank `summary`.
- `skips`: explained skip objects, or an empty array.
- `no_change`: `null` when documentation updates exist; otherwise an object with a nonblank `explanation`. Never report updates and a no-change conclusion together.

Complete librarian example:

```json
{"watchtower_stage_result":{"schema_version":1,"stage_kind":"librarian","outcome":"completed","remaining_work":[],"remaining_concerns":[],"librarian":{"reviewed_paths":["internal/stageresult/result.go","docs/guildhall/repo-layout-and-module-path.md"],"documentation_updates":[{"path":"docs/guildhall/repo-layout-and-module-path.md","summary":"documented the structured stage-result boundary"}],"skips":[],"no_change":null}}}
```
