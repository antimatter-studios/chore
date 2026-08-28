<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/fingerprint/fingerprint.go:130 -->
---
title: Skipping work that is done
summary: sources, generates, status, and --force
aliases: sources generates status force cache
---

# Skipping work that is done

Two independent checks. Either can skip a task; both are off unless declared.

```yaml
build:
  sources:   ['src/**/*.go', go.mod]
  generates: [bin/app]
  cmds:      [go build -o bin/app ./...]

ensure:db:
  status: [ 'docker inspect pg >/dev/null 2>&1' ]
  cmds:   [ docker run -d --name pg postgres ]
```

- **`sources`/`generates`** compare a CONTENT checksum against the one the last
  successful run recorded. Patterns are rendered first, so
  `sources: ['src/*.{{.EXT}}']` hashes the files it names rather than the
  pattern. Every file `generates:` matched last time must still exist, which is
  what catches a deleted binary.
- **`status`** is a list of shell commands. All exiting zero means "already
  done, skip". Use it when the evidence is not a file — a container running, a
  volume present.

`--force` runs the task regardless. A task with neither declaration always runs.

Skipping a task also skips its `deps:` and any `defer:` it would have
registered — nothing was entered, so nothing registered. Hooks are the
exception and run anyway, because a hook is not the task's prerequisite.
