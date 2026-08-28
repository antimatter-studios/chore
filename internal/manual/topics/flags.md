<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:68 -->
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
    -v, --verbose      echo commands even for silent tasks
        --no-color     plain output (also: NO_COLOR, or a non-terminal)
        --no-lifecycle turn off hooks for this run — see `chore help hooks`
    -h, --help         usage, or a task's own help when a task is named
        --version      print the version

`--file` also accepts `--taskfile`, and `--no-color` accepts `--no-colour`.

A mistyped long flag is REFUSED rather than bound to something else. That
matters more than it sounds: before 0.3.0 an unknown `--flag` was taken as a
positional argument, so a typo silently set a different parameter.
