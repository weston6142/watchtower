# guildhall init + config file — Design

Date: 2026-07-27
Status: Approved

## Problem

Today `guildhall daemon` is configured entirely by CLI flags (`--repo`, `--flows` (required), `--packages`, `--slots`, `--budget`, `--test-cmd`, …). Nothing persists a repo's setup, so every restart requires the full incantation. Worse, the default `--data` dir (`~/.local/share/guildhall`) is global while the daemon is single-repo: two repos started with defaults collide on `guildhall.sock` and the database — the second daemon removes the first one's socket.

## Goals

- `guildhall init` in a repo makes it runnable with zero flags.
- Clients auto-spawn the daemon when it isn't running.
- A global registry records initialized repos (groundwork for a future shared multi-repo daemon).
- Per-repo daemons remain the model in this step; fix the socket/DB collision.

Non-goals: a single shared daemon, launchd/systemd residency, `up/down` fleet control.

## Design

### 1. Config file — `.guildhall/config.yaml` at the repo root

Committed to git. Fields mirror the daemon flags; all optional with defaults:

```yaml
flows: .guildhall/flows        # dir of flow YAMLs
packages: .guildhall/packages  # agent package defs
runner: claude                 # claude|fake
slots: 4
budget: 0
price_per_mtok: 0
claude_bin: claude
test_cmd: "go test ./..."
```

Relative paths resolve against the repo root.

Precedence: explicit CLI flag > config file > built-in default. The repo is discovered by walking up from CWD to the directory containing `.guildhall/`; `--repo` remains as an override. `--flows` is no longer required.

### 2. `guildhall init`

Run inside a repo:

- Writes `.guildhall/config.yaml` and scaffolds starter `flows/` and `packages/` from defaults embedded in the binary (`go:embed`), so a fresh repo runs immediately and the files are editable and committable.
- Never overwrites existing files; re-running is a no-op that reports what already exists.
- Registers the repo in the global registry.
- Prints next steps ("run `guildhall tower` — the daemon starts automatically").

### 3. Per-repo data layout

Each repo gets an ID: a short hash of its absolute path. State lives under `~/.local/share/guildhall/repos/<id>/`:

```
guildhall.db
guildhall.sock
daemon.log
daemon.pid
```

The old flat layout is retired; `--data` remains only as an override of the base dir. Two repos can run daemons concurrently with zero flags — this fixes the socket collision.

### 4. Registry

`~/.local/share/guildhall/repos.d/<id>.yaml` containing `{path, registered_at}`. New command `guildhall repos` lists registered repos with live daemon status (probes each socket). Nothing acts on the registry automatically in this step.

### 5. Auto-spawn

Client dialing (`mustDial`) becomes:

1. Resolve the repo (CWD walk or `--repo`).
2. Try the repo's socket; on success, proceed.
3. If dead/absent: if the socket file exists but refuses connections and the pidfile's process is dead, remove the stale socket. Fork a detached `guildhall daemon` for the repo (stdout/stderr → `daemon.log`, pid → `daemon.pid`).
4. Poll the socket up to ~5s; print `started daemon for <repo>` and proceed.

`guildhall daemon` stays available for foreground/debug runs.

### 6. Error handling

- Client run outside any initialized repo → clear error pointing at `guildhall init`.
- No flows found in the configured dir → daemon errors with the config path named in the message.
- Spawn timeout → surface the tail of `daemon.log`.

### 7. Testing

- Unit: config load and flag precedence, repo-discovery walk, repo-ID hashing, registry read/write, init idempotency.
- Integration: `init` a temp repo → a client command auto-spawns a fake-runner daemon (`runner: fake` in config; the `GUILDHALL_FAKE=1` env override stays honored) → command succeeds. Run a second temp repo concurrently to verify no socket/DB collision.
