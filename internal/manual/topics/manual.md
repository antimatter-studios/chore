<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/manual/manual.go:27 -->
---
title: The built-in manual
summary: how `chore help` works, and how to add a topic
---

# The built-in manual

```
chore help              list the topics
chore help hooks        print one
```

Underscores and hyphens are interchangeable, because the name is a phrase
half-remembered rather than an identifier copied: `chore help lifecycle_hooks`
and `chore help lifecycle-hooks` are the same question.

## How it is built

The pages are EXTRACTED from the source. A topic section is a standalone
comment block sitting next to the code that implements it:

```go
// chore:manual hooks
// title: Hooks
// summary: before/on_success/on_failure/after, on a task or the whole run
// order: 10
//
// # Hooks
//
// ...markdown...
```

`chore manual` regenerates `internal/manual/topics/*.md`, which are embedded
into the binary. CI regenerates and fails on a diff.

- `title:` and `summary:` are written ONCE per topic, on any one of its blocks.
  Two blocks disagreeing is an error rather than a silent first-wins, which
  would make the listing depend on file order.
- `order:` sorts sections within a topic; ties break on file path, then line. A
  topic can therefore be assembled from several files and still come out the
  same way on every machine, which is what makes the CI diff meaningful.
- The block must be SEPARATED from any declaration by a blank line. Attached to
  one it becomes a Go doc comment, and gofmt reformats doc comments —
  re-indenting code blocks and rewriting list markers, which mangles markdown.

The point is that the paragraph describing a rule lives in the same diff as the
rule. A document in another file is a copy, and a copy drifts silently.
