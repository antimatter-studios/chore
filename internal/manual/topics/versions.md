<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/buildinfo/buildinfo.go:47 -->
---
title: Versions
summary: what --version reports, and pinning the chore a file needs
aliases: version chore-min-version
---

# Versions

`chore --version` prints the bare version on stdout and the context on stderr,
so a script reading the first line is never broken by the extra detail:

```
0.7.0
  commit  d8a38e2
  dated   2026-08-27 12:21 UTC (9 minutes ago)
  built   go1.25.0 darwin/arm64
  file    chores.yml
```

`dated` is the date the SOURCE was committed, not the build time — it answers
"how old is what I am running", which is the actual question when a fix appears
not to have worked.

A build from a checkout says so with a banner on stderr, because the most
expensive way to lose an hour is to test the installed copy while believing it
is the local one.

## `chore_min_version`

```yaml
chore_min_version: 0.8.0
```

Optional; absent means no restriction. It exists because a file's safety can
depend on the RUNNER, not only on what the file says: a taskfile driving money
declared its dangerous flags as strings compared to `"true"` for one reason —
chore before 0.4.0 bound an unknown `--flag` positionally and let a bool take
any value, so a typo set a different flag. Stating the floor is what lets such
a file drop the workaround instead of carrying it forever.

The strictest floor among all loaded files wins, includes included. A dev build
is exempt, having no version to judge and a banner that already says so. A
chore too old to know the field refuses the file anyway, because an unknown
top-level key is an error — so the floor fails closed even against versions
that predate it.
