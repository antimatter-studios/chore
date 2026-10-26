package chorefile

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Decode parses a Taskfile. Unknown fields are an error: a typo in a key is far
// more likely than a deliberate extension, and Task's habit of ignoring what it
// does not recognise turns a typo into silence.
// readable turns a yaml decode error into something aimed at whoever wrote the
// file rather than at whoever wrote this package. yaml.v3 reports an unknown key
// as `field dotenv not found in type chorefile.Task`, which names a Go type the
// reader has never heard of and cannot act on.
func readable(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	// `field X not found in type chorefile.Y` → `unknown field "X" in a Y`
	msg = unknownField.ReplaceAllStringFunc(msg, func(m string) string {
		g := unknownField.FindStringSubmatch(m)
		kind := strings.ToLower(g[2])
		article := "a"
		if strings.ContainsRune("aeiou", rune(kind[0])) {
			article = "an"
		}
		return fmt.Sprintf("unknown field %q in %s %s", g[1], article, kind)
	})
	msg = strings.ReplaceAll(msg, "chorefile.", "")
	return errors.New(strings.TrimSpace(msg))
}

var unknownField = regexp.MustCompile(`field ([A-Za-z0-9_-]+) not found in type chorefile\.([A-Za-z]+)`)

func Decode(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, readable(err)
	}
	if f.Silent && f.Verbose {
		return nil, fmt.Errorf("taskfile: the file sets both silent and verbose —" +
			" they are opposite answers about printing commands; keep the one you mean")
	}
	if f.ChoreMinVersion != "" {
		if _, ok := ParseSemver(f.ChoreMinVersion); !ok {
			return nil, fmt.Errorf("taskfile: chore_min_version is %q, which is not a version"+
				" — write it as MAJOR.MINOR.PATCH, as in 0.4.0", f.ChoreMinVersion)
		}
	}
	if f.Lifecycle != nil {
		for _, h := range []struct {
			name string
			cmds Cmds
		}{
			{"before_all", f.Lifecycle.BeforeAll},
			{"on_success_all", f.Lifecycle.OnSuccess},
			{"on_failure_all", f.Lifecycle.OnFailure},
			{"after_all", f.Lifecycle.AfterAll},
		} {
			if err := rejectDefer("lifecycle", h.name, h.cmds); err != nil {
				return nil, err
			}
		}
	}
	for name, t := range f.Tasks {
		if t == nil {
			// `foo:` with an empty body is legal and means "a task that does
			// nothing" — usually a placeholder or an alias target.
			f.Tasks[name] = &Task{}
			continue
		}
		// `cmd:` is one step, spelled the way go-task spells it. Folded in here so
		// that everything after the decoder sees one list of steps and never has
		// to ask which key a task used.
		if t.Single != nil {
			if len(t.Cmds) > 0 {
				return nil, fmt.Errorf("taskfile: task %q sets both `cmd:` and `cmds:` — `cmd:` is one step,"+
					" `cmds:` is a list of them; keep one", name)
			}
			t.Cmds = Cmds{*t.Single}
			t.Single = nil
		}
		for _, h := range []struct {
			name string
			cmds Cmds
		}{
			{"before", t.Before},
			{"on_success", t.OnSuccess},
			{"on_failure", t.OnFailure},
			{"after", t.After},
			{"on_timeout", t.OnTimeout},
		} {
			if err := rejectDefer("task "+quoteForError(name), h.name, h.cmds); err != nil {
				return nil, err
			}
		}
		// A handler with no clock to fire it is a teardown that will never run, and
		// the file that declares one plainly believes it will. Refused where it is
		// written: the alternative is a task that looks guarded and is not, which is
		// worse than an unguarded one because nobody looks at it twice.
		// `silent:` and `verbose:` on one task are opposite answers to the same
		// question. One would have to lose, and picking a winner silently is how a
		// file ends up meaning something nobody wrote — the failure mode this
		// decoder exists to remove. Across levels they do not conflict: a task
		// outranks its file, which is what lets one loud task live in a quiet file.
		if t.Silent && t.Verbose {
			return nil, fmt.Errorf("taskfile: task %q sets both silent and verbose —"+
				" they are opposite answers about printing its commands; keep the one you mean", name)
		}
		// A replay prints what a run printed and runs nothing, so a task whose run
		// is a conversation with a person cannot have one: there is no terminal
		// session to play back, and a question replayed is a question not asked.
		if t.Cache > 0 && t.Interactive {
			return nil, fmt.Errorf("taskfile: task %q: cache with interactive —"+
				" a terminal session cannot be replayed; drop one of them", name)
		}
		if t.Cache > 0 && t.Confirm != nil {
			return nil, fmt.Errorf("taskfile: task %q: cache with confirm —"+
				" a replay would skip the question; drop one of them", name)
		}
		if len(t.OnTimeout) > 0 && t.Timeout <= 0 {
			return nil, fmt.Errorf("taskfile: task %q: on_timeout with no timeout —"+
				" nothing would ever fire it; give the task a budget, as in `timeout: 20m`", name)
		}
		shorts := map[string]string{}
		for i, arg := range t.Args {
			if !IsName(arg.Name) {
				return nil, fmt.Errorf("taskfile: task %q: args entry %d is %s, which cannot be used as a variable —"+
					" a parameter name must start with a letter or underscore and contain only letters, digits and underscores",
					name, i+1, describeParam(arg.Name))
			}
			switch arg.Type {
			case "", TypeString, TypeBool, TypeInt:
			default:
				return nil, fmt.Errorf("taskfile: task %q: parameter %q has type %q; expected %s, %s or %s",
					name, arg.Name, arg.Type, TypeString, TypeBool, TypeInt)
			}
			if arg.Short == "" {
				continue
			}
			if !ValidShort(arg.Short) {
				return nil, fmt.Errorf("taskfile: task %q: parameter %q declares short %q;"+
					" a short flag is a single letter, as in `short: f` for -f",
					name, arg.Name, arg.Short)
			}
			// -h and --help are answered before a task is invoked, so a parameter
			// claiming h could never be reached: a declaration that cannot work is
			// refused where it is written rather than ignored at the call.
			if strings.EqualFold(arg.Short, "h") {
				return nil, fmt.Errorf("taskfile: task %q: parameter %q cannot use short %q:"+
					" -h is chore's help flag and is answered before the task runs,"+
					" so the parameter would be unreachable", name, arg.Name, arg.Short)
			}
			if other, dup := shorts[arg.Short]; dup {
				return nil, fmt.Errorf("taskfile: task %q: parameters %q and %q both declare short %q;"+
					" one letter cannot mean two things", name, other, arg.Name, arg.Short)
			}
			shorts[arg.Short] = arg.Name
		}
	}
	if err := validateRoutes(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// IsName reports whether s can be a variable: a template's {{.Name}} and a
// shell's $Name alike. The one definition, used for declared parameters, for
// the names chore exports to a script, and for `NAME=value` on a command line.
//
// A declared parameter that fails it can never be referenced as {{.Name}},
// which is always a mistake — and one particular
// mistake is easy to make: `- !config` is YAML TAG syntax, and yaml.v3 decodes it
// to the empty string rather than failing, so the parameter silently becomes
// unnameable. Rejecting it here is the difference between a clear error and a
// task that mysteriously receives nothing.
func IsName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func describeParam(s string) string {
	if s == "" {
		return "empty (a leading \"!\" is YAML tag syntax — remove it)"
	}
	return quoteForError(s)
}

// UnmarshalYAML accepts every form a variable can take:
//
//	FOO:   bar
//	FOO:   {sh: git rev-parse HEAD}
//	HOSTS: [web1, web2]
//	PORTS: {map: {web: 80, db: 5432}}
//
// Collections hold plain values only. A list of lists has no string form a
// shell could use and nothing in chore that would iterate it, so it is refused
// where it is written rather than flattened into something nobody meant.
func (v *Var) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		v.Value = s
		return nil
	case yaml.SequenceNode:
		items, err := plainValues(n, "a list variable")
		if err != nil {
			return err
		}
		v.Kind, v.List = VarList, items
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a variable", "sh", "map"); err != nil {
			return err
		}
		var m struct {
			Sh  *string   `yaml:"sh"`
			Map yaml.Node `yaml:"map"`
		}
		if err := n.Decode(&m); err != nil {
			return fmt.Errorf("line %d: a variable mapping accepts only `sh` or `map`: %w", n.Line, err)
		}
		switch {
		case m.Sh != nil && m.Map.Kind != 0:
			return fmt.Errorf("line %d: a variable sets either `sh` or `map`, not both", n.Line)
		case m.Sh != nil:
			v.Sh = *m.Sh
			return nil
		case m.Map.Kind != 0:
			entries, err := mapEntries(&m.Map)
			if err != nil {
				return err
			}
			v.Kind, v.Map = VarMap, entries
			return nil
		}
		return fmt.Errorf("line %d: a variable mapping must set `sh` or `map`", n.Line)
	default:
		return fmt.Errorf("line %d: a variable must be a value, a list, or a mapping with `sh` or `map`", n.Line)
	}
}

// plainValues reads a sequence whose every element is a plain value.
func plainValues(n *yaml.Node, what string) ([]string, error) {
	if err := checkNoNullElements(n, what); err != nil {
		return nil, err
	}
	items := make([]string, 0, len(n.Content))
	for _, el := range n.Content {
		if el.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: every item of %s is a plain value", el.Line, what)
		}
		s, err := scalarString(el)
		if err != nil {
			return nil, err
		}
		items = append(items, s)
	}
	return items, nil
}

// mapEntries reads the body of a `{map: …}` variable, keeping the order the
// entries are written in. A Go map would forget it, and the order is the one
// `for:` walks: a loop over services that starts a different one first each run
// is a loop nobody can reason about.
func mapEntries(n *yaml.Node) ([]MapEntry, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: `map` holds a mapping, as in `map: {web: 80, db: 5432}`", n.Line)
	}
	entries := make([]MapEntry, 0, len(n.Content)/2)
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || val.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: every key and value of a map variable is a plain value", k.Line)
		}
		key, err := scalarString(k)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, fmt.Errorf("line %d: map key %s appears twice", k.Line, quoteForError(key))
		}
		seen[key] = true
		value, err := scalarString(val)
		if err != nil {
			return nil, err
		}
		entries = append(entries, MapEntry{Key: key, Value: value})
	}
	return entries, nil
}

// UnmarshalYAML reads a step's `for:`, as a literal list or a mapping naming a
// variable:
//
//	for: [api, worker]
//	for: {var: HOSTS, split: ',', as: HOST}
func (f *For) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		items, err := plainValues(n, "a `for:` list")
		if err != nil {
			return err
		}
		f.Items = items
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a `for:`", "var", "split", "as"); err != nil {
			return err
		}
		var r struct {
			Var   string `yaml:"var"`
			Split string `yaml:"split"`
			As    string `yaml:"as"`
		}
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Var == "" {
			return fmt.Errorf("line %d: a `for:` mapping needs `var`, the variable to loop over", n.Line)
		}
		if !IsName(r.Var) {
			return fmt.Errorf("line %d: `for:` var %s is not a variable name — write the name, as in `var: HOSTS`,"+
				" not a template", n.Line, quoteForError(r.Var))
		}
		f.Var, f.Split, f.As = r.Var, r.Split, r.As
	case yaml.ScalarNode:
		// go-task's `for: sources` and `for: generates`. Refused by name rather
		// than read as a one-item list, which would loop once over the word.
		return fmt.Errorf("line %d: `for:` takes a list, as in `for: [a, b]`, or a variable, as in `for: {var: NAMES}`",
			n.Line)
	default:
		return fmt.Errorf("line %d: `for:` takes a list or a mapping", n.Line)
	}
	if f.As != "" && (!IsName(f.As) || f.As == "KEY") {
		return fmt.Errorf("line %d: `for:` as %s cannot hold the item — it must be a variable name other than KEY,"+
			" which a map loop sets to the key", n.Line, quoteForError(f.As))
	}
	return nil
}

// UnmarshalYAML reads a duration the way Go writes one: 20m, 90s, 1h30m.
//
// A bare number is refused rather than read as seconds. `timeout: 30` looks like
// a deadline and would be one — thirty of something — and a safety net that can
// be out by a factor of sixty because the unit was assumed is not a safety net.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalarString(n)
	if err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		// `timeout:` with nothing after it. Left as zero, which means no deadline,
		// the same as not writing the key — there is no value here to get wrong.
		return nil
	}
	v, perr := time.ParseDuration(s)
	if perr != nil {
		return fmt.Errorf("line %d: %s is not a duration — write it with a unit, as in 20m, 90s or 1h30m",
			n.Line, quoteForError(s))
	}
	if v <= 0 {
		return fmt.Errorf("line %d: a duration of %s would be spent before the task started;"+
			" remove the key to go without, or give it a real length of time", n.Line, quoteForError(s))
	}
	*d = Duration(v)
	return nil
}

// UnmarshalYAML accepts a shell command as a plain string, or a mapping calling
// another task:
//
//	cmds:
//	  - go build ./...
//	  - task: postgres:up
//	    vars: {PORT: '5432'}
func (c *Cmd) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		c.Cmd = s
		return nil
	case yaml.SequenceNode:
		// An argv: `- [kubectl, get, pods, -A]`. chore quotes it, so no word can
		// be split or reinterpreted by the shell that runs it.
		if err := checkNoNullElements(n, "an argv"); err != nil {
			return err
		}
		if len(n.Content) == 0 {
			return fmt.Errorf("line %d: an argv step needs at least the program to run", n.Line)
		}
		for _, el := range n.Content {
			if el.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: every word of an argv is a plain value", el.Line)
			}
			w, err := scalarString(el)
			if err != nil {
				return err
			}
			c.Argv = append(c.Argv, w)
		}
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a command", "cmd", "task", "vars", "silent", "ignore_error", "defer", "for"); err != nil {
			return err
		}
		type raw struct {
			// A node, so `cmd:` inside a mapping takes the same two forms as a bare
			// step: a shell line or an argv.
			Cmd         yaml.Node      `yaml:"cmd"`
			Task        string         `yaml:"task"`
			Vars        map[string]Var `yaml:"vars"`
			Silent      bool           `yaml:"silent"`
			IgnoreError bool           `yaml:"ignore_error"`
			// Defer nests a whole command: `- defer: { task: e2e:down }` or
			// `- defer: docker rm -f box`.
			Defer *Cmd `yaml:"defer"`
			For   *For `yaml:"for"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		var body Cmd
		if r.Cmd.Kind != 0 {
			if r.Cmd.Kind == yaml.MappingNode {
				return fmt.Errorf("line %d: `cmd` is a shell line or an argv list", r.Cmd.Line)
			}
			if err := body.UnmarshalYAML(&r.Cmd); err != nil {
				return err
			}
		}
		hasCmd := body.Cmd != "" || len(body.Argv) > 0
		if r.Defer != nil {
			if hasCmd || r.Task != "" {
				return fmt.Errorf("line %d: `defer` is a command of its own; do not combine it with `cmd` or `task`", n.Line)
			}
			if r.For != nil {
				// Two readings, both plausible: defer one loop, or loop over
				// registering defers. The first is the useful one and it has a
				// spelling that says so.
				return fmt.Errorf("line %d: put `for:` inside the deferred command, as in"+
					" `- defer: {for: [a, b], cmd: …}`", n.Line)
			}
			*c = *r.Defer
			c.Defer = true
			return nil
		}
		if hasCmd && r.Task != "" {
			return fmt.Errorf("line %d: a command sets either `cmd` or `task`, not both", n.Line)
		}
		if !hasCmd && r.Task == "" {
			return fmt.Errorf("line %d: a command needs `cmd`, `task` or `defer`", n.Line)
		}
		c.Cmd, c.Argv, c.Task, c.Vars = body.Cmd, body.Argv, r.Task, r.Vars
		c.Silent, c.IgnoreError, c.For = r.Silent, r.IgnoreError, r.For
		return nil
	default:
		return fmt.Errorf("line %d: a command must be a string, an argv list, or a mapping", n.Line)
	}
}

// UnmarshalYAML accepts a dependency as a name, or as a mapping passing vars:
//
//	deps: [build, {task: sign, vars: {KEY: dev}}]
func (d *Dep) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		if s == "" {
			return fmt.Errorf("line %d: empty dependency name", n.Line)
		}
		d.Task = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a dependency", "task", "vars", "silent"); err != nil {
			return err
		}
		type raw struct {
			Task   string         `yaml:"task"`
			Vars   map[string]Var `yaml:"vars"`
			Silent bool           `yaml:"silent"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Task == "" {
			return fmt.Errorf("line %d: a dependency needs `task`", n.Line)
		}
		d.Task, d.Vars, d.Silent = r.Task, r.Vars, r.Silent
		return nil
	default:
		return fmt.Errorf("line %d: a dependency must be a string or a mapping", n.Line)
	}
}

// UnmarshalYAML accepts a parameter as a bare name or as a declaration:
//
//	args:
//	  - config
//	  - {name: follow, short: f, type: bool, desc: keep streaming}
func (a *Arg) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		a.Name = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a parameter", "name", "short", "type", "desc"); err != nil {
			return err
		}
		type raw struct {
			Name  string `yaml:"name"`
			Short string `yaml:"short"`
			Type  string `yaml:"type"`
			Desc  string `yaml:"desc"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Name == "" {
			return fmt.Errorf("line %d: a parameter needs `name`", n.Line)
		}
		a.Name, a.Short, a.Type, a.Desc = r.Name, r.Short, r.Type, r.Desc
		return nil
	default:
		return fmt.Errorf("line %d: a parameter must be a name or a mapping", n.Line)
	}
}

// UnmarshalYAML rejects an empty list entry, like Cmds and Deps.
func (as *Args) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "args"); err != nil {
		return err
	}
	var out []Arg
	if err := n.Decode(&out); err != nil {
		return err
	}
	*as = out
	return nil
}

// UnmarshalYAML rejects an empty list entry. See the Cmds type comment.
func (c *Cmds) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "cmds"); err != nil {
		return err
	}
	var out []Cmd
	if err := n.Decode(&out); err != nil {
		return err
	}
	*c = out
	return nil
}

// UnmarshalYAML rejects an empty list entry. See the Cmds type comment.
func (d *Deps) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "deps"); err != nil {
		return err
	}
	var out []Dep
	if err := n.Decode(&out); err != nil {
		return err
	}
	*d = out
	return nil
}

func checkNoNullElements(n *yaml.Node, what string) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: %s must be a list", n.Line, what)
	}
	for i, el := range n.Content {
		if el.Tag == "!!null" {
			return fmt.Errorf("line %d: %s entry %d is empty — a bare \"-\" would be dropped silently", el.Line, what, i+1)
		}
	}
	return nil
}

// knownFields rejects any key of a mapping node that is not in allowed.
//
// Decode sets KnownFields(true), but yaml.Node.Decode — the only way a custom
// UnmarshalYAML can read its node — builds a fresh decoder that does NOT
// inherit it. So inside Var, Cmd and Dep strictness has to be re-imposed by
// hand, or `{sh: date, shh: 1}` and `{cmd: x, slient: true}` decode cleanly and
// the typo becomes exactly the silence Decode exists to prevent.
func knownFields(n *yaml.Node, what string, allowed ...string) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if !slices.Contains(allowed, key.Value) {
			return fmt.Errorf("line %d: unknown field %s in %s; expected one of %v",
				key.Line, quoteForError(key.Value), what, allowed)
		}
	}
	return nil
}

// scalarString renders a YAML scalar as the string the templating layer expects.
// Numbers and booleans are written unquoted all over real Taskfiles (`port: 25`,
// `silent: true` inside a vars block), and every consumer here treats a variable
// as text, so normalise rather than reject.
//
// Template text is returned byte-for-byte: a value like {{`{{.Names}}`}} passes
// a Go template through to docker's --format, and mangling it would break that.
func scalarString(n *yaml.Node) (string, error) {
	switch n.Tag {
	case "!!str", "!!int", "!!float", "!!bool", "!!timestamp":
		return n.Value, nil
	case "!!null":
		return "", nil
	default:
		var s string
		if err := n.Decode(&s); err != nil {
			return "", fmt.Errorf("line %d: expected a value, got %s", n.Line, n.Tag)
		}
		return s, nil
	}
}

// quoteForError makes a value printable inside an error message.
func quoteForError(s string) string { return strconv.Quote(s) }

// rejectDefer refuses `- defer:` inside a hook.
//
// `defer` means "run this when the surrounding task finishes", and a hook is not
// that task — it is a list that runs to completion at one point in the task's
// life. So the step has nothing to defer to. Accepting it would leave the reader
// to guess between two wrong answers: that it unwinds at the end of the hook
// (invisible, and pointless in a list this short) or at the end of the task
// (which the hook cannot reach). A hook that genuinely needs paired teardown
// calls a task that has it, where `defer:` means what it says.
func rejectDefer(where, hook string, cmds Cmds) error {
	for i, c := range cmds {
		if c.Defer {
			return fmt.Errorf("taskfile: %s: %s step %d is a `defer:` — a hook runs to completion,"+
				" so there is nothing for it to defer to; put it in `cmds:`, or call a task that does",
				where, hook, i+1)
		}
	}
	return nil
}
