<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:39 -->
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
