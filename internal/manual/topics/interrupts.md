<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:278 -->
---
title: Ctrl-C and cleanup
summary: what an interrupt stops, and what still runs afterwards
aliases: ctrl-c signals cleanup
---

# Ctrl-C and cleanup

**An interrupt stops the task, not just chore.** A script runs in its OWN
process group, which is what lets cancellation kill what the script started
rather than only the shell — but the terminal signals the FOREGROUND group,
which is chore. So SIGINT and SIGTERM are caught, cancel the run, and take the
group with them.

Without that, `chore app:run` exited instantly and left `flutter run` behind:
chore died from the default action, and the hook that would have killed the
group never fired because nothing ever cancelled the context.

Exit status is 128+signal, which is what every shell reports for a signalled
command and what a caller checking `$?` already knows how to read. An
interrupted run is reported as interrupted, not as the failure its internal
error would suggest.

**Teardown survives the interrupt.** `defer:` steps and every best-effort hook
run afterwards on a FRESH context with a bounded grace budget, because a
cancelled context cannot start a process — teardown issued on one would be
silently skipped at the exact moment it matters most.

**A second signal is not caught.** Someone pressing Ctrl-C twice has stopped
waiting for a tidy shutdown, so the default action takes over and there is
always a way out.
