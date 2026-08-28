<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/dotenv.go:11 -->
---
title: dotenv files
summary: loading KEY=VALUE files, and why a missing one is an error
aliases: env-files
---

# dotenv files

```yaml
dotenv: ['config/{{.CONFIG}}/config.env', '?config/{{.CONFIG}}/secrets.env']
```

The path may reference a parameter, because arguments bind before dotenv is
resolved. That ordering is the whole point: go-task resolves dotenv at parse
time, before command-line variables exist, so the same line there loads the
DEFAULT config and the command acts on the wrong stack while reporting success.

**A missing dotenv file is an ERROR, not a shrug.** Skipping it silently makes
every variable fall back to a default, container names resolve to `-suffix`,
and commands quietly match nothing. A partial miss is reported on stderr and
continues, because an absent `secrets.env` is normal; prefix a path with `?` to
silence even that report.

A task's own `dotenv:` REPLACES its file's, and `dotenv: []` declines them
outright — a task whose job is to hand off to a peer repository has no business
loading this project's environment. Omitting the key inherits, which is what
almost every task wants.

The format is the boring subset: comments, blank lines, optional quotes, no
interpolation and no `export` cleverness. Anything stranger is an error rather
than a guess, because a silently mis-parsed value is how a stack ends up
pointing at the wrong host.
