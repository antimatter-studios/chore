package run

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/shell"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// chore:manual loops
// title: Loops
// summary: for: — one step per item of a list, a map or a string
// aliases: for loop list lists map maps iterate
// order: 10
//
// # Loops
//
// `for:` repeats one step per item, so a loop over hosts, drivers or services is
// something chore walks rather than a bash `for` it cannot see into. The step is
// a shell line or a call to another task, exactly as it is without `for:`.
//
// ```yaml
// vars:
//   DRIVERS: [ext4, ntfs, xfs]
//   PORTS:   {map: {api: 8080, worker: 9090}}
//
// tasks:
//   build:all:
//     cmds:
//       - for: {var: DRIVERS}
//         task: build
//         vars: {DRIVER: '{{.ITEM}}'}
//
//       - for: {var: PORTS}
//         cmd: echo "{{.KEY}} listens on {{.ITEM}}"
//
//       - for: [tick, analyst, auditor]
//         cmd: launchctl unload "$HOME/Library/LaunchAgents/com.rt.{{.ITEM}}.plist"
// ```
//
// ## What to loop over
//
// ```
// for: [a, b, c]                   a literal list; each item is a template
// for: {var: NAME}                 a variable — a list, a map, or a string
// for: {var: NAME, split: ','}     a string split on ',' instead of whitespace
// for: {var: NAME, as: HOST}       the item is {{.HOST}} rather than {{.ITEM}}
// ```
//
// The item is a variable like any other, so `{{.ITEM}}` and `$ITEM` are the same
// value; a map loop sets `{{.KEY}}` and `$KEY` too. With `as: HOST` the item is
// in HOST only, and ITEM is not set. `as:` cannot be KEY.
//
// A list gives its items in order. A map gives each entry in the order it is
// written, the key in `{{.KEY}}` and the value in `{{.ITEM}}`. A string is split
// on whitespace unless `split:` says otherwise, so `{sh: ls *.sh}` output and a
// value typed on the command line both loop without further work. Items split
// out of a string are trimmed, and empty ones are skipped.
//
// A `for:` naming a variable that is not set is an ERROR, not a loop of zero
// items: a misspelt name would otherwise succeed by doing nothing. A variable
// that is set and empty loops zero times.
//
// ## Where it works
//
// On any step: in `cmds:`, in a hook (`before:`, `after:` and the rest), and in
// a `defer:`, where it goes INSIDE the deferred command — the whole loop is
// deferred, and its items run in order when the task finishes:
//
// ```yaml
// cmds:
//   - defer: {for: [web, db], cmd: 'docker rm -f {{.ITEM}}'}
// ```
//
// Written beside `defer:` instead (`- for: […]` with `defer:` on the same step)
// it is refused, because it could mean either thing.
//
// ## List and map variables
//
// ```yaml
// vars:
//   HOSTS: [web1, web2]                 # a list
//   PORTS: {map: {web: 80, db: 5432}}   # a map — `map:` because {sh: …} owns the bare mapping
// ```
//
// Each list item and map value is a template, rendered like any variable; map
// keys are taken as written. Items are plain values — a list of lists, or a map
// holding one, is refused when the file loads.
//
// Templates and the environment see a STRING form: a list's items joined by
// spaces (`web1 web2`), a map's `key=value` pairs joined by spaces. So
// `for h in {{.HOSTS}}` in an existing script keeps working when the variable
// becomes a list. A list passed to another task through `vars:` arrives as that
// string, and `for:` splits it back into the same items — exactly, as long as no
// item contains a space.
//
// A value from the command line replaces a list like any other value:
// `chore build:all DRIVERS="ext4 xfs"` loops over two drivers.
//
// A template cannot walk a list itself. `{{range .HOSTS}}` fails with "range
// can't iterate over web1 web2", and `{{index .HOSTS 0}}` prints 119, the first
// byte of the string, because a template only ever sees the string form. Loop
// with `for:`.
//
// ## Order and failure
//
// Items run one at a time, in order, and the first failure stops the loop and
// fails the step — the same rule as `cmds:` itself — with the error naming the
// item:
//
// ```
// chore: build:all: for ITEM="broken": build: exit status 1
// ```
//
// With `ignore_error: true` on the step or on the task, a failing item is
// reported and the loop carries on to the next.
//
// Each `- task:` iteration is an ordinary call: it sees only the `vars:` passed
// to it, `requires:` and `args:` check each call, and `run: once` keys on the
// rendered vars, so distinct items each run.
//
// The spelling is go-task's. Not supported: `for: sources`, `for: {matrix: …}`,
// and `for:` on a `deps:` entry.

// loop runs a step once per item of its `for:`.
//
// It lives in command, not in the cmds loop, so every place a step can appear —
// a task's cmds, a `defer:`, a hook — loops the same way without a second copy
// of the rule.
func (r *Runner) loop(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, sh shell.Shell, rem *remote, c chorefile.Cmd) error {
	items, err := loopItems(scope, c.For)
	if err != nil {
		return fmt.Errorf("%s: for: %w", t.Name, err)
	}
	body := c
	body.For = nil
	for _, it := range items {
		vars := it.vars(c.For.ItemName())
		// The shell was built from the task's scope before the loop existed, so
		// the item is added to its environment here: `$ITEM` and `{{.ITEM}}` must
		// be the same value, as every other variable's two spellings are.
		iterSh := sh
		iterSh.Env = slices.Clone(sh.Env)
		for k, v := range vars {
			iterSh.Env = append(iterSh.Env, k+"="+v)
		}
		if err := r.command(ctx, t, scope.Push(vars), iterSh, rem, body); err != nil {
			if c.IgnoreError || t.IgnoreError {
				fmt.Fprintf(r.errorOutput(ctx), "task: %s: for %s failed, ignored: %v\n", t.Name, it.label(c.For.ItemName()), err)
				continue
			}
			// Which item, said once here: a called task's own report names the
			// task, and nothing in it says which pass of the loop called it.
			return fmt.Errorf("%s: for %s: %w", t.Name, it.label(c.For.ItemName()), err)
		}
	}
	return nil
}

// loopItem is one iteration: a value, and for a map the key it sits under.
type loopItem struct {
	key, value string
	isMap      bool
}

// vars is what the iteration binds: the item, and the key for a map.
func (it loopItem) vars(name string) map[string]string {
	if it.isMap {
		return map[string]string{"KEY": it.key, name: it.value}
	}
	return map[string]string{name: it.value}
}

// label names the item in a report, by the key when there is one.
func (it loopItem) label(name string) string {
	if it.isMap {
		return "KEY=" + strconv.Quote(it.key)
	}
	return name + "=" + strconv.Quote(it.value)
}

// loopItems lists what a `for:` walks, against the scope of the task running it.
func loopItems(scope *tmpl.Scope, f *chorefile.For) ([]loopItem, error) {
	if f.Var == "" {
		out := make([]loopItem, 0, len(f.Items))
		for _, raw := range f.Items {
			v, err := scope.Render(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, loopItem{value: v})
		}
		return out, nil
	}

	v, ok := scope.Lookup(f.Var)
	if !ok {
		return nil, fmt.Errorf("variable %s is not set — a loop over nothing would succeed by doing nothing", f.Var)
	}
	switch v.Kind {
	case tmpl.List:
		out := make([]loopItem, 0, len(v.Items))
		for _, item := range v.Items {
			out = append(out, loopItem{value: item})
		}
		return out, nil
	case tmpl.Map:
		out := make([]loopItem, 0, len(v.Items))
		for i, item := range v.Items {
			out = append(out, loopItem{key: v.Keys[i], value: item, isMap: true})
		}
		return out, nil
	}
	var words []string
	if f.Split == "" {
		words = strings.Fields(v.String())
	} else {
		// Trimmed, so `a, b` split on ',' is two names rather than a name and a
		// name with a leading space no command would match.
		for _, w := range strings.Split(v.String(), f.Split) {
			if w = strings.TrimSpace(w); w != "" {
				words = append(words, w)
			}
		}
	}
	out := make([]loopItem, 0, len(words))
	for _, w := range words {
		out = append(out, loopItem{value: w})
	}
	return out, nil
}
