<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/cache.go:61 -->
---
title: Caching an answer
summary: cache: — a task's output, kept for a lifetime instead of run again
aliases: replay replays ttl lifetime
---

# Caching an answer

```yaml
queue:list:
  cache: 5m
  cmds: [ scripts/am-queue {{.CLI_ARGS}} ]
```

A task that reads something remote — an issue tracker, a CI system, a
package registry — is run again and again within minutes, and the answer
has almost never moved. `cache:` says how long it stays true. The first run
is an ordinary run, its output streaming as always, and what it writes to
stdout is recorded. For the next five minutes the task is not run: chore
prints the recording and exits 0.

- **Only a success is kept.** A run that fails records nothing, so the next
  call runs the task again rather than serving a network error as the
  answer for five minutes.
- **The whole task is cached.** Its `deps:`, hooks and `- task:` steps are
  recorded with it and are not run when the answer comes from the cache
  either. A task running alongside it is never recorded into its entry.
- **Different arguments are different answers.** An entry is kept per
  chorefile, task, arguments — what follows `--` included — the directory
  chore was run from, declared variables, and the text of its `cmds:`,
  `deps:` and `before:`. Editing the task makes the old entries unreachable.
- **The environment is not part of it.** Variables that differ from one
  shell to the next must not make every call miss, so the process
  environment is left out. A task whose answer depends on an environment
  variable should declare it in `vars:`, where the cache key can see it.
- **stderr is not cached.** Warnings belong to the run that printed them.

`--force` runs the task anyway and replaces the recording; `--dry` runs
nothing and keeps nothing. `cache:` is for tasks that read: a task whose
point is a side effect must not declare one. `cache:` with `interactive:` or
`confirm:` is refused when the file loads — there is no terminal session to
play back, and a replayed question is a question not asked.

Entries live in the user's cache directory — `$XDG_CACHE_HOME/chore/cache`,
or `~/.cache/chore/cache` — one file each, written whole or not at all, and
a project's expired entries are removed whenever it writes a new one. With
neither directory known, the task simply runs.
