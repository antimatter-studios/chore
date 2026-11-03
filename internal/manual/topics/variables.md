<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/run.go:880 -->
---
title: Variables
summary: where a value can come from, and which one wins
aliases: vars templates env
---

# Variables

Templating is Go `text/template` with `if`/`else`/`range` and exactly one added
function, `default`. No sprig.

## Precedence, lowest first

```
process environment
dotenv files
file vars:          (with the include's vars: merged in)
task vars:
vars: passed by a caller  (- task: x, or a deps: entry)
NAME=value typed on the command line
positional arguments
```

Nothing later shadows something earlier, and it is all resolved at ONE point.
That single evaluation is the fix for go-task's `dotenv:`, which resolves while
parsing — before command-line variables exist — so `task up CONFIG=x` loads the
default config's environment and acts on the wrong stack.

The one thing above a command-line variable is a value a parent passes
EXPLICITLY to a child, because a file naming a value for one step is describing
that step rather than guessing: a task that brings up two servers by passing
each its own name must not have both collapsed into one by a global.

## Six forms

```yaml
vars:
  NAME:     literal
  SHA:      {sh: git rev-parse HEAD}      # captured from a shell command
  HOSTS:    [web1, web2]                  # a list
  PORTS:    {map: {web: 80, db: 5432}}    # a map, kept in written order
  PROFILES: {json_file: '{{.TASKFILE_DIR}}/../profiles.json'}   # a config file
  HOMELAB:  {yaml_file: '?{{.TASKFILE_DIR}}/../homelab.yaml'}   # optional
```

A list or map is for `for:` to walk — see `chore help loops`. Everywhere
else it reads as a string: `{{.HOSTS}}` and `$HOSTS` are `web1 web2`, and
`{{.PORTS}}` is `web=80 db=5432`. That includes `range`: a template cannot
iterate a list, because all it is ever given is the string.

## Config files: `json_file:` and `yaml_file:`

Code and config apart: a chore file that is the same on every machine reads
what differs per machine from a file beside it. chore parses the file itself,
and the two forms give the same result — the format is never guessed from the
extension, so a YAML file named in `json_file:` is an error.

- **Templates get the tree.** `{{.PROFILES.work.directory}}`, `{{range
  .PROFILES.hosts}}`, `{{index .PROFILES "a-key"}}`. Numbers print as they
  are written (`0.50` stays `0.50`). An object or array printed whole is
  compact JSON.
- **A key the file does not have is an error**, naming it — a typo must not
  render as an empty host. Probe an optional key with `index`, which gives
  nothing rather than failing: `{{with index .HOMELAB "lan"}}…{{end}}`.
- **The environment gets compact JSON**, in the file's own order:
  `printf '%s' "$PROFILES" | jq …` still works.
- **`for:` walks the top level**: an object's keys in written order (each value
  in `{{.ITEM}}`, as text or compact JSON), or an array's items.
- **A missing file is an error** that names the variable and the path. `?`
  before the path makes a missing file an empty object instead; a file that
  exists but does not parse is always an error, `?` or not. The path is a
  template, and a relative one is taken from the chorefile's directory.
- The top of the file is an object or an array. Duplicate keys, YAML merge
  keys (`<<`) and more than one YAML document are refused.

## Provided by chore

```
{{.ROOT_DIR}}          directory of the root taskfile
{{.TASKFILE_DIR}}      directory of the file this task came from
{{.USER_WORKING_DIR}}  where the person actually was
{{.TASK}}              the task's own name
{{.CLI_ARGS}}          everything after --
{{.CHORE_EXE}}         path of the binary running, NOT what PATH answers
{{.CHORE_VERSION}}     its version
{{.EXIT_CODE}}         in `after` and `after_all` only
```

`CHORE_EXE` matters when a task must invoke chore — a launchd plist needs an
absolute path. Resolving the word `chore` through PATH answers "the one I would
get if I typed it", not "the one running me", and those differ exactly when it
matters: a file whose `env:` pins PATH, or a dev binary run from a checkout.

Every variable whose name can be one is also exported to the script as an
environment variable, so `$CONFIG` and `{{.CONFIG}}` are the same value.
