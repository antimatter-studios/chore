<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:41 -->
---
title: Running tasks
summary: how a command line is read, and where the task name ends
aliases: usage tasks cli-args
---

# Running tasks

    chore [flags] <task> [args...] [-- extra]

Flags are read up to the **first non-flag word**, and that word is the task
name. Everything after it belongs to the task:

    chore logs -f api          # -f is the TASK's, not chore's
    chore -f other.yml logs    # -f is chore's, because it came first

This is not a parser quirk to work around — it is the rule that makes a task
able to take options of its own without chore claiming them first.

Everything after `--` is left alone and arrives as `{{.CLI_ARGS}}`, so a task
can forward arbitrary words to whatever it runs.

With no task named, `chore` lists what is available — the same answer as
`chore --list`, so running the bare command is never a mystery.

`chore <task> --help` describes that task and **runs nothing**. It works in
either position: `chore --help up` and `chore up --help` are the same
question.

## Wrapping a command: `passthrough: true`

`--` is what keeps a task's words apart from chore's. Without it chore reads
`key=value` as one of its own variables and answers `--help` itself, and a
task that never reads CLI_ARGS would drop the words silently, so it is the
rule everywhere. A task that exists to wrap one command can opt out:

```yaml
pulumi:
  passthrough: true
  cmds: ['pulumi {{.CLI_ARGS}}']
```

Chore then stops reading at the task name, the way `sudo` and `env` stop at
the command they run:

    chore pulumi up --yes -c key=value     # CLI_ARGS: up --yes -c key=value
    chore pulumi up --help                 # pulumi's help, not chore's
    chore --help pulumi                    # chore's description of the task

Declared `args:` fill from the first words, positionally — `--param` and
`NAME=value` are not read — and CLI_ARGS is everything after them. A `--`
where CLI_ARGS would begin is still chore's separator and is dropped, so
`chore pulumi -- up --yes` means the same; one further along is passed on,
so `chore pulumi config set -- key -1` reaches pulumi with its `--`.

CLI_ARGS stays one string joined with spaces, as it is after `--`: a quoted
word with a space in it does not survive as one word.
