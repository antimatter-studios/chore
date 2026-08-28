# Hook examples

Ten taskfiles, each demonstrating one rule about hooks, and a `.golden` file
recording exactly what it prints.

They are not samples to copy — they are **the test suite for the documentation**.
A comment claiming `after` runs on both paths is a claim; `02-failure.golden` is
evidence, and `go test ./examples` fails the moment the claim stops being true.
Everything in `chore help hooks` is demonstrated by one of these.

| file | what it settles |
|---|---|
| `01-order.yml` | the full order, including `before` preceding `deps:` |
| `02-failure.yml` | `on_failure` then `after`; the status still stands |
| `03-before-gates.yml` | a failed gate skips `cmds:` and keeps its own status |
| `04-best-effort.yml` | a failing `after` is not fatal, a failing `defer:` is |
| `05-child-hooks.yml` | `child_hooks: false` over a subtree, and `defer:` surviving it |
| `06-lifecycle.yml` | the run level wrapping the task level |
| `07-no-lifecycle.yml` | the flag suppresses hooks, not `deps:` or `defer:` |
| `08-up-to-date.yml` | hooks fire for a skipped task; `defer:` does not |
| `09-run-once.yml` | `run: once` bounds the hooks with the task |
| `10-task-scope.yml` | a hook reads the task's own arguments and vars |

## Running them

Each file is a normal taskfile, so read one and then run it:

```
chore -f examples/hooks/01-order.yml demo
chore -f examples/hooks/05-child-hooks.yml all
chore -f examples/hooks/07-no-lifecycle.yml --no-lifecycle demo
```

The exact invocations that were recorded are listed at the top of each golden
file, and in `cases` in `examples/examples_test.go`.

## Changing them

```
go test ./examples            # verify, in process
go test ./examples -update    # re-record after a deliberate change
chore test:installed          # verify against the BUILT BINARY instead
```

The last one sets `CHORE_BIN` and execs `bin/chore` rather than calling the
library. Same golden files either way, which is the point: a difference between
the two modes is a packaging fault — a missing embed, a bad linker stamp — that
an in-process test cannot see by construction. It is what proves the built-in
manual survived the build.

A new example needs an entry in `cases`; one without is a failure, because an
example nothing executes is documentation nothing verifies.

Read the golden diff before committing an update. These files are the reason a
behaviour change cannot be made quietly — that is the point of them, and
re-recording without reading is how it stops working.
