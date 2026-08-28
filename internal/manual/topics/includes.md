<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/loader/load.go:41 -->
---
title: Includes
summary: pulling in another taskfile without variable bleed
aliases: include inherit namespaces
---

# Includes

```yaml
includes:
  monitoring:
    taskfile: tasks/monitoring.yml
    dir: ../monitoring          # where its tasks RUN
    vars: {IP: '{{.POSTGRES_IP}}'}
    optional: true
    inherit: false              # the default
```

Tasks arrive namespaced: `monitoring:prometheus:up`.

**An included file sees ONLY what is mapped to it**, plus the outside world.
This is the structural fix. go-task flattens an included file's variables into
the parent's namespace, so two includes silently overwrite each other's
values — which is why one real project could not include a second at all and
had to shell out to another `task -d` instead.

`vars:` on the include is resolved where it is WRITTEN — in the parent — so
`{IP: '{{.POSTGRES_IP}}'}` means the parent's POSTGRES_IP. Resolving it inside
the include, which deliberately sees nothing, would yield an empty string and a
host address with a hole in it.

`inherit: true` brings the including file's variables in as a layer BELOW the
file's own, so a global config can be declared once at the root without every
include listing every name — and a file still wins on any name it defines
itself. Off by default, because a file that silently sees everything above it
is the bleed this format is known for.

`optional: true` tolerates the file being absent. `flatten: true` drops the
namespace prefix.

A `lifecycle:` block in an included file is IGNORED. Only the root file's runs.
