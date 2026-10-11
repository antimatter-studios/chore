<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/preconditions.go:15 -->
---
title: Preconditions
summary: refuse a task before preparation or work starts
aliases: precondition checks
---

# Preconditions

`preconditions:` lists what must be true before a task can run. Each entry is
a shell string, `{sh: command, msg: explanation}`, or a task check:

```yaml
tasks:
  release:
    preconditions:
      - sh: git diff --quiet HEAD
        msg: 'Commit first: a release needs a clean tree'
      - task: on-main
        msg: 'Release what is on main: check out main and pull'
      - test -z "$(git tag -l v{{.VERSION}})"
    before: [./prepare-release.sh]
    cmds: [./release.sh]
  on-main:
    desc: HEAD is origin/main
    cmd: git fetch -q origin main && test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
```

Platform restrictions and `requires:` are checked first. Preconditions then
run in order, before `before:`, dependencies, up-to-date checks and commands.
The first failure refuses the task with the check's exit status. Its task
hooks and deferred steps never run; run-level failure and finish hooks do.
A task may have both `preconditions:` and `before:`: the checks permit it to
run, then `before:` prepares it.

Output is quiet unless `-v` asks for commands and their output. A custom
`msg:` is rendered in the task's scope; without one the error names the
rendered command or task. Shell checks run locally in the task's `dir:`.
Task checks accept `vars:` like a task step and use ordinary references:
bare names in this file, `:name` in the root, `global:<file>:<name>` globally.
A task check cannot have `route:`. Its result is remembered for the invocation
by task and passed variables, so shared checks run once.

Neither `--force`, `--no-lifecycle` nor `child_hooks: false` bypasses checks.
A task's `timeout:` includes them. `--dry` prints `check:` lines and executes
no checks. The shell and `{sh, msg}` forms accept go-task Taskfiles, but chore
always checks before dependencies and never bypasses them with `--force`.
