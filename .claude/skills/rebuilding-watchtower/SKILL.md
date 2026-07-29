---
name: rebuilding-watchtower
description: Use when the watchtower binary should be rebuilt and installed after code changes — "rebuild", "install the latest", "restart the daemon" — or when a running TUI/daemon shows stale behavior that recent edits should have fixed.
---

# Rebuilding Watchtower

## Overview

Reinstall the `watchtower` binary and restart its daemons so running processes pick up new code. The trap: clients auto-spawn the daemon from `os.Executable()`, so a daemon started before the reinstall keeps serving **old code** on its socket forever — reinstalling alone is not enough.

The repo still lives at `~/guildhall` — that directory name is deliberate and must not be renamed. Everything else (binary, command, data dir, socket, db) is `watchtower`.

## Steps

```bash
# 1. From the repo root: build + install onto PATH (~/go/bin)
go install ./cmd/watchtower

# 2. Stop stale daemons (one pidfile per registered repo)
kill $(cat ~/.local/share/watchtower/repos/*/daemon.pid 2>/dev/null)

# 3. Verify: any dialing command auto-respawns the daemon with the new binary,
#    then repos should report "running"
watchtower issues
watchtower repos
```

`watchtower tower`, `issues`, `status`, etc. reconnect and spawn a fresh daemon automatically — there is no explicit daemon start step. `watchtower repos` only *reports* status; it never spawns, so don't use it alone as the verify step. If `watchtower` isn't found, the install dir isn't on PATH — use `$(go env GOPATH)/bin/watchtower` (usually `~/go/bin/watchtower`).

## Notes

- TUI-only changes still need step 2 skipped? No — skip nothing. The TUI is client-side, but killing daemons is cheap and avoids guessing which side changed.
- Per-repo data lives in `~/.local/share/watchtower/repos/<repo-id>/` (`daemon.pid`, `daemon.log`, `watchtower.sock`, `watchtower.db`). Check `daemon.log` there if a daemon won't come up.
- Integration tests can leak `watchtower daemon` processes pointing at `/tmp` data dirs; `pgrep -lf "watchtower daemon"` shows them, and they're safe to kill.
- Run `go test ./...` before installing if the changes weren't already verified.
