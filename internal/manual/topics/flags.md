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

`--dry` works for a global task too, where it prints the route hop by hop and
what would run at the end of it, without touching the network — see
`chore help global`.

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

## The four settings, in the order they settle

```
a command's own silent:      never printed, --verbose included
--verbose                    prints everything else
the task's verbose:/silent:  a loud task in a quiet file, or the reverse
the file's verbose:/silent:
```

- **`verbose: true` on a task** prints its commands with nobody passing a
  flag. For the task whose commands are part of what the operator is meant to
  see: a deploy, a destructive migration, anything where "what exactly did it
  run" is the question afterwards.
- **`silent: true` on a command** is the only setting attached to THAT text,
  so it is the only one that can promise it never reaches a screen — a token on
  a command line. It outranks `--verbose`, and the failing-step report skips it
  too, because a secret does not become printable by failing.
- **A task setting both is refused when the file loads.** They are opposite
  answers to one question; picking a winner silently is how a file ends up
  meaning something nobody wrote. Across LEVELS there is no conflict, which is
  what lets one loud task live in a quiet file.
- **`silent:` on a task or a file** otherwise governs only chore's own progress
  notices now — in practice the "is up to date" line. It has never touched a
  command's own output: that is the task's business and passes through
  untouched.
