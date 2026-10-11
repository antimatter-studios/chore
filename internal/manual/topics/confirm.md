<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/confirm.go:16 -->
---
title: Confirm
summary: confirm: — ask a person before the task does its work
aliases: confirmation yes ask
---

# Confirm

`confirm:` asks the person who ran the task, once, immediately before its own
commands. Nothing runs until they answer.

```yaml
tasks:
  up:
    confirm: 'Deploy {{.STACK}}?'
    cmds: [pulumi up --yes]

  install:
    confirm:
      text: 'Install a mail server into {{.CONTEXT}}?'
      show: kubectl config get-contexts {{.CONTEXT}}
      default: no
    cmds: [./install.sh]
```

The short form is the whole feature for most tasks. `show:` runs a command
and prints its output first, for the question you can only answer after
looking at something. `default:` is the answer a bare Enter gives — `no`
unless the file says otherwise, so the key pressed without reading cannot do
anything. `y`, `yes`, `n` and `no` are accepted in any case; anything else is
asked again.

## Why it is not a precondition

`preconditions:` answers "may this run?" — a fact about the world. This
answers "do you want this?" — intent from a person. Two consequences follow
from keeping them apart, and the second is the reason:

  - `--yes` means exactly one thing. Were the question a precondition, the
    flag would have to bypass SOME preconditions and not others.
  - Preconditions run before `before:` and `deps:`. A question asked there
    would be asked before the thing that informs it — the preview, the diff,
    the context — had been produced. `confirm:` is asked last instead.

## Where it sits

```
preconditions → before → up-to-date → deps → CONFIRM → cmds → ...
```

So: everything that could refuse the task has already refused it, every
dependency has been built, and a task that was up to date never asks —
there is no point confirming work that is not going to happen.

## Answering without a keyboard

`--yes` (`-y`), or `CHORE_YES=1` for a caller whose command line you do not
control, answers yes in advance. `-v` reports each question it answered that
way.

**With no terminal and no `--yes`, the task is REFUSED** — never assumed to
be a yes. A pipeline that grew a confirm fails loudly on its next run rather
than quietly proceeding, which is the one outcome a confirmation exists to
prevent. The refusal prints the question, so the log says what was being
asked.

`--dry` prints a `confirm:` line and asks nothing, exactly as it lists
`check:` lines without running them. `--force` and `--no-lifecycle` do not
answer it: one is about up-to-date checks and the other about hooks.

## Details

The question and anything `show:` prints go to **stderr**, so a confirmation
never lands in piped output, and `chore deploy > log` still shows you the
diff and the question.

An answer is remembered for the invocation by task and rendered question, so
a confirm on a shared dependency is asked once. Questions are asked one at a
time even when concurrent `deps:` reach them together: there is one keyboard.
A failing `show:` fails the task — there is nothing left to decide on.
