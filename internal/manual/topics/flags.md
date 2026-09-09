<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:69 -->
---
title: Flags
summary: every flag chore itself takes
---

# Flags

    -C, --dir DIR      change to DIR before looking for the taskfile
    -f, --file FILE    file to read (default: chores.yml, searched upward)
    -l, --list         list tasks with their descriptions
        --dry          print what a task would run, without running it
        --force        run even if up-to-date checks say the work is done
    -v, --verbose      print each command before it runs
        --no-color     plain output (also: NO_COLOR, or a non-terminal)
        --no-lifecycle turn off hooks for this run — see `chore help hooks`
    -h, --help         usage, or a task's own help when a task is named
        --version      print the version

`--file` also accepts `--taskfile`, and `--no-color` accepts `--no-colour`.

A mistyped long flag is REFUSED rather than bound to something else. That
matters more than it sounds: before 0.3.0 an unknown `--flag` was taken as a
positional argument, so a typo silently set a different parameter.

## Commands are not printed unless you ask

`--verbose` prints each command before it runs. Nothing does otherwise — a
task's script is written for the shell, not for a reader, and a `case`
dispatcher or a pipeline with a saved status in the middle of it is several
lines of noise printed AHEAD of the work, where it cannot be skipped past and
buries the output that was wanted. Up to 0.10.1 it was printed by default and
every one of this project's own eleven curated examples switched it off, which
is a fair verdict on the default.

What that default was worth is kept: when a step fails, the step is printed
then — after its own output, on stderr, and only when the failure is not being
ignored. So a five-step task still says which line produced the status.

`--dry` prints the commands INSTEAD of running them, and is unaffected.
`silent:` on a task or a file is now only about chore's own progress notices,
which is to say the "is up to date" line.
